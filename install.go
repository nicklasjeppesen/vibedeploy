package main

import (
	"bufio"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------- «vibedeploy install»: sætter serveren op, efter install.sh har installeret Docker og Caddy ----------
//
// Som «once background install» i ONCE: binæren opretter selv sin tjeneste. Kommandoen kan køres igen
// sikkert; den overskriver tjenestefil og Caddyfile, men aldrig nøglen til hemmelighederne.

//go:embed deploy/vibedeploy.service
var serviceTemplate string

//go:embed deploy/Caddyfile
var caddyTemplate string

//go:embed deploy/vibedeploy-update.service
var updateServiceTemplate string

//go:embed deploy/vibedeploy-update.path
var updatePathTemplate string

const (
	etcDir       = "/etc/vibedeploy"
	keyFilePath  = etcDir + "/secret.key"
	envFilePath  = etcDir + "/env"
	servicePath  = "/etc/systemd/system/vibedeploy.service"
	unitDir      = "/etc/systemd/system/"
	caddyPath    = "/etc/caddy/Caddyfile"
	binaryPath   = "/usr/local/bin/vibedeploy"
	placeholder  = "example.com"
	placeholderE = "din@email.dk"
)

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,}$`)

// installConfig er det, install skal vide. Felterne kommer fra flag, miljøvariabler eller spørgsmål i terminalen.
type installConfig struct {
	Domain, Email, SetupCode string
	codeChosen               bool // ejeren valgte selv koden (ellers er den lavet og skal vises)
}

// runInstall er «vibedeploy install». Den kræver root og Linux med systemd.
func runInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	domain := fs.String("domain", os.Getenv("VIBEDEPLOY_DOMAIN"), "dit domæne, fx min-server.dk (eller VIBEDEPLOY_DOMAIN)")
	email := fs.String("email", os.Getenv("VIBEDEPLOY_EMAIL"), "e-mail til Let's Encrypt (eller VIBEDEPLOY_EMAIL)")
	fs.Parse(args)

	fail := func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "\nFEJL: "+f+"\n", a...)
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fail("kør som root: sudo vibedeploy install")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fail("install kræver Linux med systemd")
	}

	cfg := installConfig{Domain: *domain, Email: *email, SetupCode: os.Getenv("VIBEDEPLOY_SETUP_CODE")}
	if err := cfg.ask(); err != nil {
		fail("%v", err)
	}
	if err := cfg.validate(); err != nil {
		fail("%v", err)
	}

	steps := []struct {
		name string
		f    func() error
	}{
		{"Bruger vibedeploy", ensureUser},
		{"Nøgle til hemmeligheder (" + keyFilePath + ")", func() error { _, err := loadKey(keyFilePath, true); return err }},
		{"Opsætningskode (" + envFilePath + ")", func() error { return writeEnvFile(cfg.SetupCode) }},
		{"Tjeneste (" + servicePath + ")", func() error { return os.WriteFile(servicePath, []byte(renderService(cfg)), 0o644) }},
		{"Caddy (" + caddyPath + ")", func() error { return os.WriteFile(caddyPath, []byte(renderCaddyfile(cfg)), 0o644) }},
		{"Opdatering (" + unitDir + updatePathUnit + ")", writeUpdateUnits},
		{"Start", startServices},
	}
	for _, s := range steps {
		fmt.Println("==> " + s.name)
		if err := s.f(); err != nil {
			fail("%s: %v", s.name, err)
		}
	}

	fmt.Printf("\nVibe Deploy er installeret.\n\n")
	fmt.Printf("  1. Peg DNS på serveren: en A-post for %s og en for *.%s\n", cfg.Domain, cfg.Domain)
	fmt.Printf("  2. Åbn http://SERVERENS-IP for at se, hvornår DNS og https er klar\n")
	fmt.Printf("  3. Opret din konto på https://%s/signup\n", cfg.Domain)
	if !cfg.codeChosen {
		fmt.Printf("\n  Opsætningskode: %s\n  Den skal bruges, når kontoen oprettes. Skriv den ned nu.\n", cfg.SetupCode)
	}
	fmt.Printf("\n  Gem en kopi af %s et sikkert sted. Uden den kan hemmelighederne ikke læses efter en genetablering.\n", keyFilePath)
}

// ask spørger i terminalen om det, der mangler. Uden terminal (fx cloud-init) gives fejl i stedet.
func (c *installConfig) ask() error {
	c.codeChosen = c.SetupCode != ""
	if c.SetupCode == "" { // køres install igen, beholdes koden fra sidst
		if b, err := os.ReadFile(envFilePath); err == nil {
			c.SetupCode = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "VIBEDEPLOY_SETUP_CODE="))
		}
	}
	if c.Domain != "" && c.Email != "" {
		if c.SetupCode == "" {
			c.SetupCode = randomToken(8)
		}
		return nil
	}
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return errors.New("angiv -domain og -email (eller VIBEDEPLOY_DOMAIN og VIBEDEPLOY_EMAIL), når der ikke er en terminal")
	}
	defer tty.Close()
	in := bufio.NewReader(tty)
	read := func(q string) string {
		fmt.Print(q)
		s, _ := in.ReadString('\n')
		return strings.TrimSpace(s)
	}
	for c.Domain == "" {
		c.Domain = read("Dit domæne (fx min-server.dk; apps får navn.min-server.dk): ")
	}
	for c.Email == "" {
		c.Email = read("Din e-mail (til Let's Encrypt): ")
	}
	if c.SetupCode == "" {
		c.SetupCode = randomToken(8)
	}
	return nil
}

// emailRe accepterer kun almindelige ASCII-tegn. Let's Encrypt afviser e-mails med skjulte eller
// ikke-ASCII-tegn (fx et usynligt tegn fra copy/paste), og så kan der aldrig hentes et certifikat.
var emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)

// validate tjekker domæne, e-mail og opsætningskode.
func (c *installConfig) validate() error {
	c.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(c.Domain), "https://"), "http://"), "/"))
	if !domainRe.MatchString(c.Domain) {
		return fmt.Errorf("«%s» ligner ikke et domæne (skriv fx min-server.dk)", c.Domain)
	}
	if c.Domain == placeholder {
		return errors.New("skriv dit eget domæne")
	}
	c.Email = strings.TrimSpace(c.Email)
	if !emailRe.MatchString(c.Email) || c.Email == placeholderE {
		return fmt.Errorf("«%s» er ikke en gyldig e-mail. Skriv den med almindelige bogstaver (ingen skjulte tegn fra copy/paste, ingen æøå)", c.Email)
	}
	if c.SetupCode == "vælg-en-lang-kode-selv" {
		return errors.New("vælg din egen opsætningskode (VIBEDEPLOY_SETUP_CODE)")
	}
	if len(c.SetupCode) < 12 || strings.ContainsAny(c.SetupCode, "\n\r") {
		return errors.New("opsætningskoden skal være mindst 12 tegn")
	}
	return nil
}

// renderService er deploy/vibedeploy.service med domænet indsat.
func renderService(c installConfig) string {
	return strings.ReplaceAll(dropHowTo(serviceTemplate), placeholder, c.Domain)
}

// renderCaddyfile er deploy/Caddyfile med domæne og e-mail indsat.
func renderCaddyfile(c installConfig) string {
	return strings.ReplaceAll(strings.ReplaceAll(dropHowTo(caddyTemplate), placeholder, c.Domain), placeholderE, c.Email)
}

// dropHowTo fjerner kommentarerne til den manuelle vej («Gem som …», «Erstat example.com …»).
func dropHowTo(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "# Gem som") || strings.HasPrefix(l, "# Erstat") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// writeUpdateUnits skriver de to systemd-enheder, der installerer nye versioner (se update.go).
func writeUpdateUnits() error {
	if err := os.WriteFile(unitDir+updateServiceUnit, []byte(dropHowTo(updateServiceTemplate)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(unitDir+updatePathUnit, []byte(dropHowTo(updatePathTemplate)), 0o644)
}

// ensureUser opretter systembrugeren vibedeploy, hvis den ikke findes.
func ensureUser() error {
	if exec.Command("id", "vibedeploy").Run() == nil {
		return nil
	}
	return run("useradd", "--system", "--home", "/var/lib/vibedeploy", "--shell", "/usr/sbin/nologin", "vibedeploy")
}

// writeEnvFile gemmer opsætningskoden, som tjenesten læser (EnvironmentFile). Kun root kan læse filen.
func writeEnvFile(code string) error {
	if err := os.MkdirAll(etcDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(envFilePath, []byte("VIBEDEPLOY_SETUP_CODE="+code+"\n"), 0o600)
}

// startServices genindlæser systemd, starter Vibe Deploy og genindlæser Caddy.
func startServices() error {
	if exe, err := os.Executable(); err == nil {
		if p, _ := filepath.EvalSymlinks(exe); p != binaryPath {
			fmt.Printf("    Bemærk: tjenesten kører %s, men denne kommando er %s\n", binaryPath, p)
		}
	}
	for _, c := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "vibedeploy"},
		{"systemctl", "enable", "--now", updatePathUnit},
		{"systemctl", "restart", "vibedeploy"},
	} {
		if err := run(c[0], c[1:]...); err != nil {
			return err
		}
	}
	if run("systemctl", "reload", "caddy") != nil {
		return run("systemctl", "restart", "caddy")
	}
	return nil
}

// run kører en kommando og tager dens output med i fejlen.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
