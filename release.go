package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ---------- Signerede releases ----------
//
// Hver release har en manifest-fil (release.json) med versionen og SHA-256 af hver binær, og en signatur
// (release.json.sig) lavet med en ed25519-nøgle. Serveren stoler kun på en release, hvis signaturen passer
// med den offentlige nøgle, der er bygget ind i programmet (releasePublicKey i releasekey.go).
// Versionen står inde i den signerede fil, så ingen kan narre serveren til en ældre version.

// signContext lægges foran manifestet, før det signeres, så signaturen ikke kan genbruges til andet.
const signContext = "vibedeploy-release-v1\n"

// releaseRepo er GitHub-repoet, releases hentes fra. Release-workflowet sætter det til sit eget repo
// (-ldflags "-X main.releaseRepo=ejer/repo"), så en fork opdaterer fra sig selv.
var releaseRepo = "nicklasjeppesen/vibedeploy"

// Manifest beskriver én release: version, link til ændringerne og SHA-256 (hex) pr. fil.
type Manifest struct {
	Version string            `json:"version"`
	Created time.Time         `json:"created"`
	Notes   string            `json:"notes,omitempty"`
	Files   map[string]string `json:"files"`
}

// binaryName er filnavnet på binæren til denne maskine, fx vibedeploy-linux-amd64.
func binaryName() string { return "vibedeploy-" + runtime.GOOS + "-" + runtime.GOARCH }

// updateBase er adressen, releases hentes fra. VIBEDEPLOY_UPDATE_URL kan pege et andet sted hen (fx et spejl).
func updateBase() string {
	if u := strings.TrimSpace(os.Getenv("VIBEDEPLOY_UPDATE_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://github.com/" + releaseRepo + "/releases/latest/download"
}

// fileBase er adressen til filerne i netop den release, manifestet beskriver. «latest» kan skifte mellem
// to hentninger, så binæren hentes fra release-taggets egen adresse, når det er GitHub.
func fileBase(base, ver string) string {
	if strings.HasSuffix(base, "/releases/latest/download") {
		return strings.TrimSuffix(base, "/releases/latest/download") + "/releases/download/" + ver
	}
	return base
}

// verifyManifest tjekker signaturen med pub og læser manifestet.
func verifyManifest(pub ed25519.PublicKey, raw []byte, sigB64 string) (*Manifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("der er ingen offentlig nøgle til releases i denne version (releasekey.go), så opdateringer kan ikke tjekkes")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("signaturen kan ikke læses")
	}
	if !ed25519.Verify(pub, append([]byte(signContext), raw...), sig) {
		return nil, errors.New("signaturen passer ikke: releasen er ikke lavet med projektets nøgle")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("release.json kan ikke læses: %w", err)
	}
	if _, ok := parseSemver(m.Version); !ok {
		return nil, fmt.Errorf("ugyldig version i release.json: %q", m.Version)
	}
	return &m, nil
}

// trustedKey er den indbyggede offentlige nøgle (nil, hvis der ikke er nogen).
func trustedKey() ed25519.PublicKey {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(releasePublicKey))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil
	}
	return b
}

// httpGetLimited henter url og afviser svar større end limit.
func httpGetLimited(client *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s svarede %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s er for stor", url)
	}
	return b, nil
}

// fetchRelease henter og tjekker den nyeste release fra base.
func fetchRelease(client *http.Client, base string, pub ed25519.PublicKey) (*Manifest, error) {
	raw, err := httpGetLimited(client, base+"/release.json", 64<<10)
	if err != nil {
		return nil, err
	}
	sig, err := httpGetLimited(client, base+"/release.json.sig", 4<<10)
	if err != nil {
		return nil, err
	}
	return verifyManifest(pub, raw, string(sig))
}

// downloadVerified henter filen name fra releasen til dst og tjekker SHA-256 mod manifestet.
// dst skrives kun, hvis alt passer.
func downloadVerified(client *http.Client, base string, m *Manifest, name, dst string) error {
	want, ok := m.Files[name]
	if !ok {
		return fmt.Errorf("releasen %s har ingen %s", m.Version, name)
	}
	resp, err := client.Get(fileBase(base, m.Version) + "/" + name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hentning af %s: %s", name, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".vibedeploy-hent-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 300<<20))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(want) {
		return fmt.Errorf("%s er ikke den fil, releasen beskriver (SHA-256 %s, ventede %s, %d bytes)", name, got, want, n)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// ---------- vibedeploy keygen / sign ----------

// runKeygen er «vibedeploy keygen»: laver et nøglepar til at signere releases med.
func runKeygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "release-signing.key", "fil til den hemmelige nøgle")
	fs.Parse(args)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("%v", err)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fatalf("kunne ikke oprette %s (findes den allerede?): %v", *out, err)
	}
	fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv))
	if err := f.Close(); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf(`Nøglepar lavet.

Hemmelig nøgle: %s
  Læg indholdet i en GitHub-secret med navnet VIBEDEPLOY_SIGNING_KEY (Settings → Secrets → Actions),
  og gem en kopi et sikkert sted. Den må aldrig i git.

Offentlig nøgle (sæt den ind i releasekey.go):
  var releasePublicKey = %q
`, *out, base64.StdEncoding.EncodeToString(pub))
}

// runSign er «vibedeploy sign»: skriver release.json og release.json.sig for binærerne i en mappe.
// Nøglen læses fra -key eller miljøvariablen VIBEDEPLOY_SIGNING_KEY.
func runSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyFile := fs.String("key", "", "fil med den hemmelige nøgle (ellers VIBEDEPLOY_SIGNING_KEY)")
	ver := fs.String("version", "", "versionen, fx v0.3.0")
	notes := fs.String("notes", "", "link til ændringerne")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fatalf("brug: vibedeploy sign -version v0.3.0 [-key fil] [-notes url] <mappe med binærer>")
	}
	dir := fs.Arg(0)
	keyText := os.Getenv("VIBEDEPLOY_SIGNING_KEY")
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fatalf("%v", err)
		}
		keyText = string(b)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyText))
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		fatalf("ingen gyldig hemmelig nøgle (brug -key eller VIBEDEPLOY_SIGNING_KEY)")
	}
	raw, err := buildManifest(dir, *ver, *notes, time.Now().UTC())
	if err != nil {
		fatalf("%v", err)
	}
	pub := ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)
	if tk := trustedKey(); tk != nil && !tk.Equal(pub) {
		fatalf("nøglen passer ikke med releasePublicKey i releasekey.go; serverne ville afvise releasen")
	}
	if trustedKey() == nil {
		fmt.Fprintln(os.Stderr, "Advarsel: releasekey.go har ingen offentlig nøgle, så servere med denne version kan ikke tjekke releasen.")
	}
	sig := ed25519.Sign(ed25519.PrivateKey(priv), append([]byte(signContext), raw...))
	if err := os.WriteFile(filepath.Join(dir, "release.json"), raw, 0o644); err != nil {
		fatalf("%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release.json.sig"), []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("Signeret %s: release.json og release.json.sig i %s\n", *ver, dir)
}

// buildManifest laver release.json for alle vibedeploy-<os>-<arch>-filer i dir.
func buildManifest(dir, ver, notes string, created time.Time) ([]byte, error) {
	if _, ok := parseSemver(ver); !ok {
		return nil, fmt.Errorf("-version skal være som v0.3.0, ikke %q", ver)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "vibedeploy-*-*"))
	m := Manifest{Version: ver, Created: created, Notes: notes, Files: map[string]string{}}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		m.Files[filepath.Base(p)] = hex.EncodeToString(sum[:])
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("ingen vibedeploy-<os>-<arch>-filer i %s", dir)
	}
	return json.MarshalIndent(m, "", "  ")
}

// fatalf skriver en fejl og afslutter.
func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FEJL: "+f+"\n", a...)
	os.Exit(1)
}
