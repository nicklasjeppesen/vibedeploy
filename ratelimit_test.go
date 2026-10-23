package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeClock er et ur, testene selv flytter.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClockedLimiter() (*loginLimiter, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	l := newLoginLimiter()
	l.now = c.now
	return l, c
}

func TestLimiterPerAccount(t *testing.T) {
	l, c := newClockedLimiter()
	for i := 1; i < loginMaxPerAccount; i++ {
		if _, acc := l.fail("1.1.1."+string(rune('0'+i)), "Nicklas@live.dk"); acc {
			t.Fatalf("låst for tidligt efter %d forsøg", i)
		}
	}
	// Sidste forsøg fra endnu en IP: kontoen låses, uanset hvor forsøgene kommer fra, og uden forskel på store/små bogstaver.
	if _, acc := l.fail("9.9.9.9", "nicklas@LIVE.dk"); !acc {
		t.Fatal("kontoen skal låses efter grænsen")
	}
	if l.blocked("8.8.8.8", "nicklas@live.dk") <= 0 {
		t.Fatal("kontoen skal være låst fra alle IP'er")
	}
	if l.blocked("8.8.8.8", "mette@example.dk") != 0 {
		t.Fatal("andre konti må ikke låses")
	}
	c.add(loginLockout + time.Second)
	if l.blocked("8.8.8.8", "nicklas@live.dk") != 0 {
		t.Fatal("låsen skal udløbe")
	}
}

func TestLimiterPerIPAndWindow(t *testing.T) {
	l, c := newClockedLimiter()
	// Forsøg spredt over mere end vinduet tæller ikke sammen.
	for i := 0; i < loginMaxPerAccount-1; i++ {
		l.fail("2.2.2.2", "a@b.dk")
	}
	c.add(loginWindow + time.Minute)
	if _, acc := l.fail("2.2.2.2", "a@b.dk"); acc {
		t.Fatal("gamle forsøg uden for vinduet må ikke tælle med")
	}
	// Mange konti fra samme IP: IP'en låses.
	locked := false
	for i := 0; i < loginMaxPerIP; i++ {
		ip, _ := l.fail("3.3.3.3", "konto"+string(rune('a'+i))+"@b.dk")
		locked = locked || ip
	}
	if !locked || l.blocked("3.3.3.3", "ny@b.dk") <= 0 {
		t.Fatal("IP'en skal låses efter grænsen")
	}
	if l.blocked("4.4.4.4", "ny@b.dk") != 0 {
		t.Fatal("andre IP'er må ikke låses")
	}
	// Et rigtigt login nulstiller kontoens tæller.
	for i := 0; i < loginMaxPerAccount-1; i++ {
		l.fail("5.5.5."+string(rune('0'+i)), "c@d.dk")
	}
	l.success("c@d.dk")
	if _, acc := l.fail("5.5.5.9", "c@d.dk"); acc {
		t.Fatal("et rigtigt login skal nulstille kontoens tæller")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 7.7.7.7")
	if got := clientIP(r); got != "7.7.7.7" {
		t.Fatalf("bag Caddy skal den sidste adresse i X-Forwarded-For bruges, fik %s", got)
	}
	r.RemoteAddr = "8.8.8.8:5000"
	if got := clientIP(r); got != "8.8.8.8" {
		t.Fatalf("direkte forbindelser må ikke kunne vælge deres IP med headeren, fik %s", got)
	}
}

func TestLoginLockout(t *testing.T) {
	s, _, _ := newResetServer(t) // Nicklas med adgangskoden «gammel-kode-123»
	l, c := newClockedLimiter()
	s.limit = l
	login := func(pw string) *httptest.ResponseRecorder {
		form := url.Values{"email": {"nicklas@live.dk"}, "password": {pw}}
		r := httptest.NewRequest("POST", "https://min-server.dk/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("X-Forwarded-For", "6.6.6.6")
		w := httptest.NewRecorder()
		s.loginSubmit(w, r)
		return w
	}
	for i := 1; i < loginMaxPerAccount; i++ {
		if w := login("forkert"); !strings.Contains(w.Body.String(), "Forkert e-mail eller adgangskode") {
			t.Fatalf("forsøg %d: forventede almindelig fejl", i)
		}
	}
	if w := login("forkert"); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "For mange forkerte forsøg") {
		t.Fatalf("efter grænsen skal login låses: %d", w.Code)
	}
	if w := login("gammel-kode-123"); w.Code != http.StatusTooManyRequests {
		t.Fatal("mens kontoen er låst, må heller ikke den rigtige adgangskode virke")
	}
	c.add(loginLockout + time.Second)
	if w := login("gammel-kode-123"); w.Code != http.StatusSeeOther {
		t.Fatalf("efter låsen skal den rigtige adgangskode virke: %d", w.Code)
	}
}
