package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseImage(t *testing.T) {
	ok := map[string]string{
		"nginx":                                   "nginx:latest",
		"nginx:1.27":                              "nginx:1.27",
		"louislam/uptime-kuma:1":                  "louislam/uptime-kuma:1",
		"docker.io/library/nginx":                 "library/nginx:latest",
		"ghcr.io/ejer/app:v2":                     "ghcr.io/ejer/app:v2",
		"registry.gitlab.com/a/b/c:main":          "registry.gitlab.com/a/b/c:main",
		"nginx@sha256:" + strings.Repeat("a", 64): "nginx@sha256:" + strings.Repeat("a", 64),
	}
	for in, want := range ok {
		r, err := parseImage(in)
		if err != nil || r.String() != want {
			t.Errorf("%q: fik %q, %v; ventede %q", in, r.String(), err, want)
		}
	}
	if r, _ := parseImage("ghcr.io/a/b"); r.DockerHub() || r.authKey() != "ghcr.io" {
		t.Errorf("ghcr.io burde ikke være Docker Hub: %+v", r)
	}
	if txt := registryLoginText(ImageRef{Host: "ghcr.io", Path: "a/b", Tag: "latest"}, "x"); !strings.Contains(txt, "read:packages") {
		t.Errorf("ghcr-vejledningen skal nævne read:packages: %s", txt)
	}
	if r, _ := parseImage("a/b"); !r.DockerHub() {
		t.Error("a/b burde være Docker Hub")
	}
	bad := []string{"", "-x", "Nginx", "a b", "localhost:5000/a", "localhost/a", "10.0.0.1/a/b", "reg.io:5000/a",
		"a/../b", "a:", "a:-x", "a@sha256:abc", "a;id", "a//b", "--help"}
	for _, in := range bad {
		if _, err := parseImage(in); err == nil {
			t.Errorf("%q burde afvises", in)
		}
	}
}

func TestEnvForSkipsRegistryLogin(t *testing.T) {
	env := envFor(App{Port: 80, Secrets: []KV{{RegistryUserKey, "u"}, {RegistryTokenKey, "t"}, {"DB_PASSWORD", "p"}}})
	if _, ok := env[RegistryTokenKey]; ok {
		t.Error("registry-token må ikke sendes ind i containeren")
	}
	if _, ok := env[RegistryUserKey]; ok {
		t.Error("registry-brugernavn må ikke sendes ind i containeren")
	}
	if env["DB_PASSWORD"] != "p" {
		t.Error("appens egne hemmeligheder skal med")
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(st, newDemoRT(), dir)
}

func waitStatus(t *testing.T, m *Manager, slug, want string) App {
	t.Helper()
	for i := 0; i < 100; i++ {
		if a, _ := m.get(slug); a.Status == want {
			return a
		}
		time.Sleep(50 * time.Millisecond)
	}
	a, _ := m.get(slug)
	t.Fatalf("status %q, ventede %q (%s)", a.Status, want, a.StatusText)
	return a
}

func TestDeployImageFlow(t *testing.T) {
	m := newTestManager(t)

	// Ny privat app: oprettes, venter på godkendelse og får tomme login-felter.
	res, err := m.DeployImage("Min app", "firma/app", 0, true, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsOK || res.App.FromImage != "firma/app:latest" || !needsRegistryLogin(res.App) {
		t.Fatalf("uventet resultat: %+v", res.App)
	}
	if err := m.SetSecret("min-app", RegistryUserKey, "u"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSecret("min-app", RegistryTokenKey, "t"); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("min-app"); err != nil {
		t.Fatal(err)
	}
	a := waitStatus(t, m, "min-app", StatusLive)
	if a.Current != 1 || a.Versions[0].Image == "" || a.Versions[0].From != "firma/app:latest" {
		t.Fatalf("version 1 burde køre fra imaget: %+v", a.Versions)
	}

	// Godkendt app med login sat: ny version hentes med det samme.
	if _, err := m.DeployImage("Min app", "firma/app:v2", 0, true, "", "u1"); err != nil {
		t.Fatal(err)
	}
	if a = waitStatus(t, m, "min-app", StatusLive); a.Current != 2 {
		t.Fatalf("version 2 burde køre, fik %d", a.Current)
	}
	m.stopAll("min-app")

	// Godkendt app, hvor login mangler: intet hentes, før ejeren har udfyldt det.
	if _, err := m.DeployImage("Anden", "nginx", 0, false, "", "u1"); err != nil {
		t.Fatal(err)
	}
	_ = m.Approve("anden")
	waitStatus(t, m, "anden", StatusLive)
	m.stopAll("anden")
	_, err = m.DeployImage("Anden", "firma/hemmelig", 0, true, "", "u1")
	var nl *ErrNeedLogin
	if !errors.As(err, &nl) {
		t.Fatalf("ventede ErrNeedLogin, fik %v", err)
	}
	if a, _ := m.get("anden"); len(a.Versions) != 1 || !needsRegistryLogin(a) {
		t.Fatalf("der må ikke laves en ny version, og felterne skal oprettes: %+v", a)
	}
}
