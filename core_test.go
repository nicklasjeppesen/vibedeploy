package main

import (
	"encoding/hex"
	"net/http"
	"testing"
)

func TestPBKDF2Vector(t *testing.T) {
	// Kendt testvektor: PBKDF2-HMAC-SHA256("password","salt",1,32)
	got := hex.EncodeToString(pbkdf2([]byte("password"), []byte("salt"), 1, 32))
	want := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	got = hex.EncodeToString(pbkdf2([]byte("password"), []byte("salt"), 4096, 32))
	want = "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"
	if got != want {
		t.Fatalf("4096 iter: got %s want %s", got, want)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Min bageri-app":     "min-bageri-app",
		"Bælgfrugter & Grød": "baelgfrugter-groed",
		"  Æble  ":           "aeble",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCheckPath(t *testing.T) {
	for _, bad := range []string{"../x", "/etc/passwd", "a/../b", "a//b", "", "a\\b", "."} {
		if checkPath(bad) == nil {
			t.Errorf("%q burde afvises", bad)
		}
	}
	for _, ok := range []string{"Dockerfile", "src/main.go", "static/a.b.css"} {
		if err := checkPath(ok); err != nil {
			t.Errorf("%q burde være ok: %v", ok, err)
		}
	}
}

func TestSecretish(t *testing.T) {
	for _, k := range []string{"STRIPE_KEY", "API_KEY", "DB_PASSWORD", "GITHUB_TOKEN", "KEY", "ADGANGSKODE"} {
		if !secretish.MatchString(k) {
			t.Errorf("%s burde ligne en hemmelighed", k)
		}
	}
	for _, k := range []string{"BUTIKSNAVN", "VALUTA", "PORT", "MONKEY_NAME"} {
		if secretish.MatchString(k) {
			t.Errorf("%s er en almindelig variabel", k)
		}
	}
}

func TestAccessRules(t *testing.T) {
	admin := &User{ID: "a", Role: RoleOwner}
	owner := &User{ID: "o", Role: RoleMember, Builder: true}
	friend := &User{ID: "f", Role: RoleMember}
	other := &User{ID: "x", Role: RoleMember}
	app := &App{OwnerID: "o", Visibility: VisPrivate}

	check := func(name string, got, want bool) {
		t.Helper()
		if got != want {
			t.Errorf("%s: fik %v, ventede %v", name, got, want)
		}
	}
	check("admin åbner privat", canOpen(app, admin), true)
	check("ejer åbner privat", canOpen(app, owner), true)
	check("ven åbner privat", canOpen(app, friend), false)
	check("ven styrer ikke", canManage(app, friend), false)
	check("admin styrer alt", canManage(app, admin), true)

	app.Visibility, app.Access = VisShared, []string{"f"}
	check("delt: udvalgt ven", canOpen(app, friend), true)
	check("delt: anden", canOpen(app, other), false)
	app.Visibility = VisMembers
	check("alle med konto", canOpen(app, other), true)
	app.Visibility = ""
	check("tom synlighed = privat", canOpen(app, other), false)
	check("kan bygge: ejer", canBuild(owner), true)
	check("kan bygge: ven", canBuild(friend), false)
}

func TestStripCookie(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "vd_app", Value: "hemmelig"})
	r.AddCookie(&http.Cookie{Name: "min_app", Value: "ok"})
	stripCookie(r, appCookie)
	if _, err := r.Cookie("vd_app"); err == nil {
		t.Fatal("vd_app burde være fjernet")
	}
	if c, err := r.Cookie("min_app"); err != nil || c.Value != "ok" {
		t.Fatal("appens egen cookie skal bevares")
	}
}
