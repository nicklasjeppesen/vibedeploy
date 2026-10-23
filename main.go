// Vibe Deploy – prototype af en selvhostet udgivelsestjeneste til vibe-kodede apps.
// Kør:  go run . -runtime demo     (uden Docker)
//
//	go run . -runtime docker   (rigtige containere)
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// Server samler alt, HTTP-handlerne har brug for: tilstand, app-styring, runtime og sideskabeloner.
type Server struct {
	st        *Store
	mgr       *Manager
	mon       *Monitor
	upd       *Updater
	rt        Runtime
	pages     map[string]*template.Template
	domain    string
	addr      string
	publicURL string

	resetKey []byte // signerer forespørgsler fra «vibedeploy reset-password» (reset.go)

	codeMu    sync.Mutex
	setupCode string // kræves til første konto, når serveren ikke kører på localhost
}

// main læser flag, åbner tilstanden, vælger runtime (docker eller demo), genstarter apps,
// der kørte sidst, og starter HTTP-serveren. Ctrl+C lukker pænt ned.
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			runInstall(os.Args[2:])
			return
		case "update":
			runUpdate(os.Args[2:])
			return
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		case "reset-password":
			runResetPassword(os.Args[2:])
			return
		case "keygen":
			runKeygen(os.Args[2:])
			return
		case "sign":
			runSign(os.Args[2:])
			return
		}
	}
	addr := flag.String("addr", "127.0.0.1:8080", "adresse serveren lytter på")
	domain := flag.String("domain", "localhost", "hoveddomæne; apps får <navn>.<domæne>")
	dataDir := flag.String("data", "./data", "mappe til data (konti, apps, logs)")
	rtName := flag.String("runtime", "docker", "docker (rigtige containere) eller demo (ingen Docker)")
	public := flag.String("public-url", "", "offentlig adresse, hvis du bruger en tunnel, fx https://abc.trycloudflare.com")
	keyFile := flag.String("key-file", "", "fil med nøglen, hemmeligheder krypteres med (standard: $CREDENTIALS_DIRECTORY/secret.key eller <data>/secret.key)")
	flag.Parse()

	abs, _ := filepath.Abs(*dataDir)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		log.Fatal(err)
	}
	kp, create := keyPath(*keyFile, abs)
	key, err := loadKey(kp, create)
	if err != nil {
		log.Fatalf("Nøglen til hemmeligheder kunne ikke læses: %v", err)
	}
	if create && *domain != "localhost" {
		fmt.Printf("Bemærk: nøglen til hemmeligheder ligger i data-mappen (%s). Flyt den uden for, så en backup af data-mappen ikke indeholder den (se DEPLOY.md).\n", kp)
	}
	box, err := newSecretBox(key)
	if err != nil {
		log.Fatal(err)
	}
	st, err := OpenStore(abs, box)
	if err != nil {
		log.Fatal(err)
	}

	var rt Runtime
	switch *rtName {
	case "demo":
		rt = newDemoRT()
	case "docker":
		if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
			log.Fatalf("Docker blev ikke fundet eller kører ikke (%s).\nInstallér og start Docker, eller kør med -runtime demo for at prøve flowet uden containere.", strings.TrimSpace(string(out)))
		}
		rt = &dockerRT{dataDir: abs}
	default:
		log.Fatal("ukendt -runtime; brug docker eller demo")
	}

	s := &Server{st: st, rt: rt, domain: *domain, addr: *addr, publicURL: strings.TrimRight(*public, "/")}
	s.resetKey = resetMACKey(key)
	s.mgr = NewManager(st, rt, abs)
	s.mon = NewMonitor(abs, rt)
	s.upd = NewUpdater(abs, st)
	s.pages = loadPages()
	if len(st.Snapshot().Users) == 0 {
		if code := strings.TrimSpace(os.Getenv("VIBEDEPLOY_SETUP_CODE")); code != "" {
			s.setupCode = code // valgt af ejeren, fx i opstartsscriptet (cloud-init)
			fmt.Println("\nOpsætningskoden er den, du valgte i opstartsscriptet (VIBEDEPLOY_SETUP_CODE).")
		} else if *domain != "localhost" {
			s.setupCode = randomToken(8)
			fmt.Printf("\nOpsætningskode (kræves, når den første konto oprettes): %s\n", s.setupCode)
		}
	}
	s.mgr.Reconcile()
	go s.mgr.DailyBackups()
	go s.mon.Run()
	go s.upd.Run()

	srv := &http.Server{Addr: *addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		// Ctrl+C lokalt, SIGTERM fra systemd (fx under en opdatering): færdiggør igangværende forespørgsler.
		// Appenes containere kører videre; næste start overtager dem (Reconcile).
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	fmt.Printf("\nVibe Deploy %s kører (%s)\n  Dashboard:   http://%s\n  MCP-adresse: http://%s/mcp\n  Apps:        http://<navn>.%s%s\n\n",
		version, rt.Name(), displayHost(*addr, *domain), displayHost(*addr, *domain), *domain, portSuffix(*addr, *domain))
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// keyPath finder nøglefilen: -key-file, ellers systemd's credentials-mappe (LoadCredential=secret.key),
// ellers <data>/secret.key. Kun den sidste oprettes automatisk; de andre skal findes.
func keyPath(flagVal, dataDir string) (path string, create bool) {
	if flagVal != "" {
		return flagVal, false
	}
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
		if p := filepath.Join(d, "secret.key"); fileExists(p) {
			return p, false
		}
	}
	return filepath.Join(dataDir, "secret.key"), true
}

// fileExists er sand, hvis path findes.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// displayHost giver «domæne:port» til opstartsbeskeden.
func displayHost(addr, domain string) string {
	_, port, _ := net.SplitHostPort(addr)
	return domain + ":" + port
}

// portSuffix giver «:port» på localhost, hvor der ikke er en proxy foran.
// På en rigtig server tager Caddy sig af port 80/443, så der tilføjes intet.
func portSuffix(addr, domain string) string {
	if domain != "localhost" {
		return ""
	}
	_, port, _ := net.SplitHostPort(addr)
	return ":" + port
}

// loadPages parser hver side i web/ sammen med layout.html, så hver side bliver en færdig skabelon.
// Skabelonerne er indlejret i programmet (go:embed), så binæren kan køre alene.
func loadPages() map[string]*template.Template {
	funcs := template.FuncMap{"rel": relTime, "statusLabel": statusLabel, "statusClass": statusClass}
	m := map[string]*template.Template{}
	entries, _ := fs.ReadDir(webFS, "web")
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".html") || n == "layout.html" {
			continue
		}
		m[n] = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+n))
	}
	return m
}
