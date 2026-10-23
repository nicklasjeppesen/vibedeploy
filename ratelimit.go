package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------- Grænse for forkerte loginforsøg ----------
//
// To tællere for forkerte forsøg inden for loginWindow:
//   - pr. IP-adresse (loginMaxPerIP): stopper én maskine, der gætter på mange konti.
//   - pr. e-mail (loginMaxPerAccount): stopper gætteri på én konto fra mange IP-adresser.
// Når en grænse er nået, afvises login fra den IP eller til den konto i loginLockout, også med den rigtige
// adgangskode. Tælleren for e-mail tæller også e-mails, der ikke findes, så svaret ikke afslører, hvilke
// konti der er. Et vellykket login nulstiller kontoens tæller (ikke IP'ens, så en angriber ikke kan nulstille
// sin IP ved at logge ind på sin egen konto ind imellem).
// Et link til ny adgangskode virker stadig for en låst konto, så ejeren aldrig bliver låst helt ude.
// Tællerne ligger i hukommelsen og starter forfra, når Vibe Deploy genstarter.

const (
	loginWindow        = 15 * time.Minute
	loginLockout       = 15 * time.Minute
	loginMaxPerIP      = 20
	loginMaxPerAccount = 5
)

// failCount er de forkerte forsøg for én nøgle (en IP eller en e-mail).
type failCount struct {
	n           int
	first       time.Time // starten på det nuværende vindue
	lockedUntil time.Time
}

// loginLimiter holder styr på forkerte forsøg. now kan udskiftes i tests.
type loginLimiter struct {
	mu   sync.Mutex
	m    map[string]*failCount
	now  func() time.Time
	last time.Time // sidste oprydning
}

// newLoginLimiter laver en tom limiter.
func newLoginLimiter() *loginLimiter {
	return &loginLimiter{m: map[string]*failCount{}, now: time.Now}
}

// keys er tællernøglerne for et forsøg: én for IP'en og én for e-mailen (uden forskel på store/små bogstaver).
func loginKeys(ip, email string) (string, string) {
	return "ip:" + ip, "konto:" + strings.ToLower(strings.TrimSpace(email))
}

// blocked returnerer, hvor længe endnu IP'en eller kontoen er låst (0 = ikke låst).
func (l *loginLimiter) blocked(ip, email string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	ik, ak := loginKeys(ip, email)
	for _, k := range []string{ik, ak} {
		if c := l.m[k]; c != nil && now.Before(c.lockedUntil) {
			if d := c.lockedUntil.Sub(now); d > wait {
				wait = d
			}
		}
	}
	return wait
}

// fail tæller et forkert forsøg og låser IP'en eller kontoen, hvis grænsen nås. Returnerer, hvad der blev låst.
func (l *loginLimiter) fail(ip, email string) (ipLocked, accountLocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanup(now)
	ik, ak := loginKeys(ip, email)
	bump := func(k string, max int) bool {
		c := l.m[k]
		if c == nil || now.Sub(c.first) > loginWindow {
			c = &failCount{first: now}
			l.m[k] = c
		}
		c.n++
		if c.n >= max && !now.Before(c.lockedUntil) {
			c.lockedUntil = now.Add(loginLockout)
			c.n, c.first = 0, now // efter låsen starter et nyt vindue
			return true
		}
		return false
	}
	return bump(ik, loginMaxPerIP), bump(ak, loginMaxPerAccount)
}

// success nulstiller kontoens tæller efter et rigtigt login.
func (l *loginLimiter) success(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ak := loginKeys("", email)
	delete(l.m, ak)
}

// cleanup fjerner gamle tællere højst én gang i minuttet, så kortet ikke vokser uden grænse.
func (l *loginLimiter) cleanup(now time.Time) {
	if now.Sub(l.last) < time.Minute {
		return
	}
	l.last = now
	for k, c := range l.m {
		if now.Sub(c.first) > loginWindow && !now.Before(c.lockedUntil) {
			delete(l.m, k)
		}
	}
}

// limiter er serverens loginLimiter (laves første gang, den bruges).
func (s *Server) limiter() *loginLimiter {
	s.limitOnce.Do(func() {
		if s.limit == nil {
			s.limit = newLoginLimiter()
		}
	})
	return s.limit
}

// clientIP er den besøgendes IP-adresse. Bag Caddy (forespørgslen kommer fra 127.0.0.1) bruges den sidste
// adresse i X-Forwarded-For, som Caddy selv sætter; ellers forbindelsens egen adresse. En besøgende kan altså
// ikke selv vælge sin IP ved at sende headeren direkte.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if p := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(p) != nil {
				return p
			}
		}
	}
	return host
}

// lockedText er beskeden, når login er låst i d endnu.
func lockedText(d time.Duration) string {
	min := int(d.Round(time.Minute) / time.Minute)
	if min < 1 {
		min = 1
	}
	when := fmt.Sprintf("om %d minutter", min)
	if min == 1 {
		when = "om et minut"
	}
	return "For mange forkerte forsøg. Prøv igen " + when + ". Har du glemt adgangskoden, så bed ejeren om et link til en ny (eller kør «sudo vibedeploy reset-password» på serveren, hvis du er ejeren)."
}
