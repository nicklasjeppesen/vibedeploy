package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"
)

// ---------- statusside under installationen ----------
//
// Efter installationen med cloud-init (deploy/cloud-init.yaml) kender ejeren kun serverens IP-adresse.
// Caddy sender http://<IP> videre hertil, og siden viser, om domænet peger på serveren, og om https virker,
// og hvornår man kan oprette sin konto. Opsætningskoden vises aldrig: den valgte ejeren selv i scriptet,
// så ingen andre kan nå at oprette kontoen først.

// SetupCheck er ét punkt på statussiden.
type SetupCheck struct {
	Name, Detail string
	OK           bool
}

// isIPHost er sand, når forespørgslen er sendt til en IP-adresse i stedet for et navn.
func isIPHost(host string) bool { return net.ParseIP(strings.Trim(host, "[]")) != nil }

// setupStatus viser statussiden. Når den første konto er oprettet, sendes man i stedet til dashboardet.
func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	base := "https://" + s.domain
	if s.publicURL != "" {
		base = s.publicURL
	}
	if len(s.st.Snapshot().Users) > 0 {
		http.Redirect(w, r, base, http.StatusSeeOther)
		return
	}
	ip := strings.Trim(hostOnly(r.Host), "[]")
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	checks := []SetupCheck{
		{Name: "Vibe Deploy kører", OK: true, Detail: "Docker, Caddy og Vibe Deploy er installeret."},
		dnsCheck(ctx, s.domain, ip, s.domain),
		dnsCheck(ctx, "test-"+randomToken(3)+"."+s.domain, ip, "*."+s.domain+" (apps)"),
	}
	https := SetupCheck{Name: "https-certifikat", Detail: "Venter på, at domænet peger på serveren."}
	if checks[1].OK {
		https = httpsCheck(ctx, base)
	}
	checks = append(checks, https)
	ready := true
	for _, c := range checks {
		ready = ready && c.OK
	}
	s.page(w, r, "setup.html", "Installation", map[string]any{
		"Checks": checks, "Ready": ready, "Signup": base + "/signup", "Domain": s.domain, "IP": ip, "RefreshSecs": 15,
	})
}

// dnsCheck slår name op og tjekker, at det peger på ip.
func dnsCheck(ctx context.Context, name, ip, label string) SetupCheck {
	c := SetupCheck{Name: "DNS for " + label}
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil || len(addrs) == 0 {
		c.Detail = "Navnet findes ikke endnu. Opret en A-post, der peger på " + ip + ". Det kan tage op til en time, før den virker."
		return c
	}
	for _, a := range addrs {
		if a == ip {
			c.OK, c.Detail = true, "Peger på denne server ("+ip+")."
			return c
		}
	}
	c.Detail = "Peger på " + strings.Join(addrs, ", ") + " i stedet for " + ip + ". Ret A-posten hos din domæneudbyder."
	return c
}

// httpsCheck henter base over https. Første gang beder Caddy Let's Encrypt om et certifikat, hvilket
// kan tage et halvt minut; siden opdaterer sig selv imens.
func httpsCheck(ctx context.Context, base string) SetupCheck {
	c := SetupCheck{Name: "https-certifikat"}
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/login", nil)
	resp, err := client.Do(req)
	if err != nil {
		c.Detail = "Ikke klar endnu. Caddy henter certifikatet, så snart DNS virker; det tager typisk under et minut."
		return c
	}
	resp.Body.Close()
	c.OK, c.Detail = true, base+" virker med https."
	return c
}
