package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// postGitNew sender formularen «Ny app fra git» direkte til handleren.
func postGitNew(t *testing.T, s *Server, u *User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/apps/new/git", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.gitNewSubmit(w, r, u)
	return w
}

func TestGitNewForm(t *testing.T) {
	m := newTestManager(t)
	s := &Server{st: m.st, mgr: m, pages: loadPages()}
	owner := &User{ID: "u1", Name: "Anna", Role: RoleOwner, Builder: true}

	// Formularen vises.
	w := httptest.NewRecorder()
	s.gitNewPage(w, httptest.NewRequest("GET", "/apps/new/git", nil), owner)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `action="/apps/new/git"`) {
		t.Fatalf("formularen vises ikke: %d", w.Code)
	}

	// Mangler repo, forkert port og optaget navn giver en forståelig fejl og beholder det, der er skrevet.
	w = postGitNew(t, s, owner, url.Values{"name": {"Blog"}})
	if !strings.Contains(w.Body.String(), "Skriv, hvor koden ligger") {
		t.Fatal("manglende repo skal forklares")
	}
	w = postGitNew(t, s, owner, url.Values{"repo": {"ejer/repo"}, "port": {"abc"}})
	if !strings.Contains(w.Body.String(), "Porten skal være et tal") || !strings.Contains(w.Body.String(), `value="ejer/repo"`) {
		t.Fatal("forkert port skal forklares, og repoet skal blive stående")
	}
	if _, err := m.Deploy("Blog", 8080, map[string]string{"index.html": "hej"}, "", owner.ID); err != nil {
		t.Fatal(err)
	}
	w = postGitNew(t, s, owner, url.Values{"repo": {"ejer/repo"}, "name": {"Blog"}})
	if !strings.Contains(w.Body.String(), "findes allerede en app") {
		t.Fatal("optaget navn skal forklares")
	}

	// Et ugyldigt repo giver en fejl i formularen, ikke en tom side.
	w = postGitNew(t, s, owner, url.Values{"repo": {"ikke et repo"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `class="note bad"`) {
		t.Fatalf("ugyldigt repo skal give en fejl i formularen: %d", w.Code)
	}

	// En konto, der ikke må bygge, afvises.
	guest := &User{ID: "g", Name: "Gæst", Role: RoleMember}
	if w = postGitNew(t, s, guest, url.Values{"repo": {"ejer/repo"}}); w.Code != http.StatusForbidden {
		t.Fatalf("en konto uden byggerettigheder skal afvises, fik %d", w.Code)
	}
}

func TestRepoDefaultName(t *testing.T) {
	for in, want := range map[string]string{"ejer/min-app": "min-app", "https://github.com/ejer/chat.git": "chat", "git@gitlab.com:g/u/app.git": "app", "nonsens": ""} {
		if got := repoDefaultName(in); got != want {
			t.Errorf("%q: fik %q, ventede %q", in, got, want)
		}
	}
}

// fakeGit lægger en falsk «git» først i PATH. Den kloner offentlige repos (adresser med «/pub/») og
// private kun over ssh; alt andet svarer «Repository not found», som GitHub gør for private repos.
func fakeGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
for a; do last=$a; prev2=$prev; prev=$a; done
url=$prev2
case "$url" in
  *"/pub/"*|git@*) mkdir -p "$last" && printf 'FROM scratch\n' > "$last/Dockerfile" && mkdir -p "$last/.git"; exit 0;;
esac
echo "fatal: Repository not found." >&2; exit 128
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGitNewPublicAndPrivate(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen mangler")
	}
	fakeGit(t)
	m := newTestManager(t)
	s := &Server{st: m.st, mgr: m, pages: loadPages()}
	owner := &User{ID: "u1", Name: "Anna", Role: RoleOwner, Builder: true}

	// Offentligt repo: appen oprettes og venter på godkendelse; navnet kommer fra repoet.
	w := postGitNew(t, s, owner, url.Values{"repo": {"pub/min-app"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/apps/min-app?m=gitnew" {
		t.Fatalf("offentligt repo: %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	a, ok := m.get("min-app")
	if !ok || a.Status != StatusWaiting || a.Git == nil || a.Git.Repo != "https://github.com/pub/min-app.git" {
		t.Fatalf("appen er ikke oprettet rigtigt: %+v", a)
	}

	// Privat repo, første gang: serveren laver en nøgle og viser den i formularen.
	w = postGitNew(t, s, owner, url.Values{"repo": {"firma/hemmelig"}, "port": {"3000"}})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "ssh-ed25519") || !strings.Contains(body, "deploy key") || !strings.Contains(body, `value="firma/hemmelig"`) {
		t.Fatalf("privat repo skal vise en deploy key og beholde det skrevne: %d %s", w.Code, body)
	}
	if _, ok := m.get("hemmelig"); ok {
		t.Fatal("der må ikke oprettes en app, før koden er hentet")
	}
	// Anden gang: nøglen findes, og (falsk) ssh-adgang virker nu, så appen oprettes.
	w = postGitNew(t, s, owner, url.Values{"repo": {"firma/hemmelig"}, "port": {"3000"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("efter at nøglen er tilføjet skal appen oprettes: %d %s", w.Code, w.Body.String())
	}
	if a, _ := m.get("hemmelig"); a.Port != 3000 || a.OwnerID != "u1" {
		t.Fatalf("port eller ejer er forkert: %+v", a)
	}
}

// TestTLSAskOnIP: Caddy spørger på http://127.0.0.1:8080/internal/tls-ask, før den henter et certifikat
// til en app. Svaret må ikke være en omdirigering (Caddy følger dem ikke), ellers får ingen app https.
func TestTLSAskOnIP(t *testing.T) {
	m := newTestManager(t)
	s := &Server{st: m.st, mgr: m, pages: loadPages(), domain: "min-server.dk"}
	if _, err := m.Deploy("Testapp", 8080, map[string]string{"index.html": "hej"}, "", "u1"); err != nil {
		t.Fatal(err)
	}
	ask := func(domain string) int {
		req := httptest.NewRequest("GET", "http://127.0.0.1:8080/internal/tls-ask?domain="+domain, nil)
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for d, want := range map[string]int{"testapp.min-server.dk": 200, "min-server.dk": 200, "findes-ikke.min-server.dk": 404, "testapp.andet.dk": 404} {
		if got := ask(d); got != want {
			t.Errorf("tls-ask for %s: %d, ventede %d", d, got, want)
		}
	}
}
