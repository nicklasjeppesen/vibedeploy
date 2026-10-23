package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setFakeSHA bestemmer, hvilken commit den falske git (fakeGit) svarer med.
func setFakeSHA(t *testing.T, file, sha string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(sha), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGitCommitsAndManualCheck(t *testing.T) {
	fakeGit(t)
	shaFile := filepath.Join(t.TempDir(), "sha")
	t.Setenv("FAKE_GIT_SHA_FILE", shaFile)
	c1, c2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	setFakeSHA(t, shaFile, c1)

	m := newTestManager(t)
	if _, err := m.DeployFromGit("Chat", "pub/chat", "", 8080, "", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("chat"); err != nil {
		t.Fatal(err)
	}
	a := waitStatus(t, m, "chat", StatusLive)
	if currentCommit(a) != c1 || a.Git.Latest != c1 || !gitState(a)["UpToDate"].(bool) {
		t.Fatalf("commit'en skal huskes på versionen: %+v %+v", a.Versions, a.Git)
	}

	// En ny commit på git ses først, når ejeren tjekker, og der bygges aldrig noget af sig selv.
	setFakeSHA(t, shaFile, c2)
	time.Sleep(100 * time.Millisecond)
	if a, _ := m.get("chat"); a.Git.Latest != c1 || len(a.Versions) != 1 {
		t.Fatal("serveren må ikke tjekke git eller bygge af sig selv")
	}
	if sha, err := m.CheckGit("chat"); err != nil || sha != c2 {
		t.Fatalf("tjek: %q %v", sha, err)
	}
	a, _ = m.get("chat")
	if len(a.Versions) != 1 || a.Git.Latest != c2 || !gitState(a)["NewOnGit"].(bool) || a.Git.Checked.IsZero() {
		t.Fatalf("efter tjek skal den nye commit vises, men ikke bygges: %d versioner, %+v", len(a.Versions), a.Git)
	}

	// Et repo, der ikke kan læses, giver en fejl, som vises på siden.
	_ = m.st.Do(func(db *DB) error { db.Apps["chat"].Git.Repo = "https://github.com/firma/hemmelig.git"; return nil })
	if _, err := m.CheckGit("chat"); err == nil {
		t.Fatal("et privat repo uden nøgle skal give en fejl")
	}
	if a, _ := m.get("chat"); a.Git.CheckErr == "" || a.Git.Latest != c2 {
		t.Fatalf("fejlen skal gemmes, og den sidst sete commit beholdes: %+v", a.Git)
	}
}

func TestGitDeployButtonSkipsWhenUpToDate(t *testing.T) {
	fakeGit(t)
	shaFile := filepath.Join(t.TempDir(), "sha")
	t.Setenv("FAKE_GIT_SHA_FILE", shaFile)
	setFakeSHA(t, shaFile, strings.Repeat("a", 40))
	m := newTestManager(t)
	s := &Server{st: m.st, mgr: m, pages: loadPages()}
	owner := &User{ID: "u1", Name: "Anna", Role: RoleOwner, Builder: true}
	if _, err := m.DeployFromGit("Chat", "pub/chat", "", 8080, "", "u1"); err != nil {
		t.Fatal(err)
	}
	_ = m.Approve("chat")
	waitStatus(t, m, "chat", StatusLive)

	press := func(form url.Values) string {
		r := httptest.NewRequest("POST", "/apps/chat/gitdeploy", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetPathValue("slug", "chat")
		r.SetPathValue("action", "gitdeploy")
		w := httptest.NewRecorder()
		s.appAction(w, r, owner)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("knappen: %d %s", w.Code, w.Body.String())
		}
		return w.Header().Get("Location")
	}
	if loc := press(url.Values{}); !strings.Contains(loc, "m=gituptodate") {
		t.Fatalf("uden noget nyt skal knappen sige, at appen er opdateret: %s", loc)
	}
	if a, _ := m.get("chat"); len(a.Versions) != 1 {
		t.Fatal("der må ikke bygges, når der ikke er noget nyt")
	}
	if loc := press(url.Values{"force": {"1"}}); !strings.Contains(loc, "m=gitfetched") {
		t.Fatalf("«Byg igen alligevel» skal bygge: %s", loc)
	}
	waitStatus(t, m, "chat", StatusLive)
	if a, _ := m.get("chat"); len(a.Versions) != 2 {
		t.Fatal("«Byg igen alligevel» skal lave en ny version")
	}

	// Siden viser kørende og nyeste commit.
	a, _ := m.get("chat")
	page := httptest.NewRecorder()
	s.page(page, httptest.NewRequest("GET", "/apps/chat?tab=indstillinger", nil), "app.html", "Chat", map[string]any{
		"A": s.view(a), "Tab": "indstillinger", "Git": gitState(a)})
	if b := page.Body.String(); !strings.Contains(b, "aaaaaaa") || !strings.Contains(b, "kører den nyeste version") {
		t.Fatalf("siden skal vise commit og status:\n%s", b)
	}
}

func TestParseLsRemote(t *testing.T) {
	sha := strings.Repeat("f", 40)
	if got, err := parseLsRemote("warning: x\n" + sha + "\trefs/heads/main\n"); err != nil || got != sha {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parseLsRemote(""); err == nil {
		t.Fatal("tomt svar betyder, at grenen ikke findes")
	}
}
