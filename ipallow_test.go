package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseAllowIPs(t *testing.T) {
	got, err := parseAllowIPs("203.0.113.7\n192.168.1.77/24, 2001:db8::1 ;::ffff:10.0.0.5\r\n203.0.113.7\n\n::ffff:10.1.0.0/112")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.7", "192.168.1.0/24", "2001:db8::1", "10.0.0.5", "10.1.0.0/16"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fik %v, ville have %v", got, want)
	}
	if got, err := parseAllowIPs("  \n "); err != nil || len(got) != 0 {
		t.Fatalf("tom tekst skal give tom liste: %v %v", got, err)
	}
	for _, bad := range []string{"min-pc", "300.1.1.1", "10.0.0.0/33", "0.0.0.0/0", "::/0", "10.0.0.1-10.0.0.9"} {
		if _, err := parseAllowIPs(bad); err == nil {
			t.Errorf("%q burde afvises", bad)
		}
	}
	var many []string
	for i := 0; i <= maxAllowIPs; i++ {
		many = append(many, "10.0.0."+strconv.Itoa(i))
	}
	if _, err := parseAllowIPs(strings.Join(many, "\n")); err == nil {
		t.Error("for mange adresser burde afvises")
	}
}

func TestIPAllowed(t *testing.T) {
	allow := []string{"203.0.113.7", "192.168.1.0/24", "2001:db8::/32"}
	for ip, want := range map[string]bool{
		"203.0.113.7":        true,
		"::ffff:203.0.113.7": true,
		"203.0.113.8":        false,
		"192.168.1.200":      true,
		"192.168.2.1":        false,
		"2001:db8:abcd::9":   true,
		"2001:db9::1":        false,
		"fe80::1%eth0":       false,
		"ikke-en-adresse":    false,
		"":                   false,
	} {
		if got := ipAllowed(allow, ip); got != want {
			t.Errorf("ipAllowed(%q) = %v, ville have %v", ip, got, want)
		}
	}
	if !ipAllowed(nil, "198.51.100.1") {
		t.Error("tom liste skal tillade alle")
	}
}

// TestProxyAppIPAllow: kun adresser på listen når frem til containeren, også for en offentlig app,
// og en falsk X-Forwarded-For fra den besøgende hjælper ikke (Caddy tilføjer den rigtige adresse sidst).
func TestProxyAppIPAllow(t *testing.T) {
	m := newTestManager(t)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "fra containeren") }))
	defer be.Close()
	u, _ := url.Parse(be.URL)
	port, _ := strconv.Atoi(u.Port())
	_ = m.st.Do(func(db *DB) error {
		db.Apps["blog"] = &App{Slug: "blog", Name: "Blog", OwnerID: "o", Visibility: VisPublic, Status: StatusLive, Current: 1, HostPort: port}
		return nil
	})
	s := &Server{st: m.st, mgr: m, domain: "min-server.dk", publicURL: "https://min-server.dk"}

	get := func(remote, xff string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://blog.min-server.dk/", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		s.proxyApp(rec, req, "blog")
		return rec
	}

	if rec := get("127.0.0.1:4000", "198.51.100.1"); rec.Code != 200 {
		t.Fatalf("uden begrænsning: status %d", rec.Code)
	}
	allow, err := parseAllowIPs("203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetAllowIPs("blog", allow); err != nil {
		t.Fatal(err)
	}
	if a, _ := m.get("blog"); !reflect.DeepEqual(a.AllowIPs, []string{"203.0.113.0/24"}) {
		t.Fatalf("listen blev ikke gemt: %v", a.AllowIPs)
	}

	if rec := get("127.0.0.1:4000", "203.0.113.9"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "fra containeren") {
		t.Fatalf("tilladt adresse bag Caddy: status %d", rec.Code)
	}
	if rec := get("127.0.0.1:4000", "203.0.113.9, 198.51.100.1"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "198.51.100.1") {
		t.Fatalf("forfalsket X-Forwarded-For slap igennem: status %d", rec.Code)
	}
	if rec := get("198.51.100.1:4000", "203.0.113.9"); rec.Code != http.StatusForbidden {
		t.Fatalf("X-Forwarded-For fra en fremmed forbindelse må ikke stoles på: status %d", rec.Code)
	}
	if rec := get("203.0.113.50:4000", ""); rec.Code != 200 {
		t.Fatalf("direkte forbindelse fra tilladt adresse: status %d", rec.Code)
	}

	if err := m.SetAllowIPs("blog", nil); err != nil {
		t.Fatal(err)
	}
	if rec := get("198.51.100.1:4000", ""); rec.Code != 200 {
		t.Fatalf("efter tom liste skal alle kunne åbne appen: status %d", rec.Code)
	}
}

// TestAddMyIPButton: «Tilføj min adresse» lægger den besøgendes IP til feltets indhold, uden dubletter.
func TestAddMyIPButton(t *testing.T) {
	s, owner, _ := newResetServer(t)
	_ = s.st.Do(func(db *DB) error {
		db.Apps["blog"] = &App{Slug: "blog", Name: "Blog", OwnerID: owner.ID, Visibility: VisPublic, AllowIPs: []string{"198.51.100.0/24"}}
		return nil
	})
	post := func(ips string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/apps/blog/ipaddme", strings.NewReader(url.Values{"ips": {ips}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set("X-Forwarded-For", "203.0.113.42")
		r.SetPathValue("slug", "blog")
		r.SetPathValue("action", "ipaddme")
		w := httptest.NewRecorder()
		s.appAction(w, r, owner)
		return w
	}
	if w := post("198.51.100.0/24\n10.0.0.1"); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	a, _ := s.mgr.get("blog")
	if want := []string{"198.51.100.0/24", "10.0.0.1", "203.0.113.42"}; !reflect.DeepEqual(a.AllowIPs, want) {
		t.Fatalf("fik %v, ville have %v", a.AllowIPs, want)
	}
	post(strings.Join(a.AllowIPs, "\n"))
	if a, _ = s.mgr.get("blog"); len(a.AllowIPs) != 3 {
		t.Fatalf("adressen må ikke komme på to gange: %v", a.AllowIPs)
	}
}
