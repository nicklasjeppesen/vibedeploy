package main

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// IP-begrænsning pr. app: når App.AllowIPs ikke er tom, sender proxyApp kun forespørgsler
// videre fra de adresser og områder, der står i listen. Det gælder for alle, også ejeren, og
// kommer oveni synligheden (login). Dashboardet påvirkes ikke, så ejeren kan altid rette listen.
//
// Adressen er den, clientIP finder: forbindelsens adresse eller, bag Caddy, sidste led i
// X-Forwarded-For. Står der en anden proxy foran (fx Cloudflare), ser serveren dens adresser.

// maxAllowIPs er det største antal adresser og områder i en apps liste.
const maxAllowIPs = 64

// parseAllowIPs læser en liste over IP-adresser og CIDR-områder adskilt af linjeskift, komma
// eller mellemrum. Tom tekst giver en tom liste (ingen begrænsning). Adresserne normaliseres
// (IPv4 skrevet som IPv6 bliver IPv4, områder får nul i værtsbittene), og dubletter fjernes.
func parseAllowIPs(text string) ([]string, error) {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	var out, bad []string
	seen := map[string]bool{}
	for _, f := range fields {
		norm, err := normAllowIP(f)
		if err != nil {
			bad = append(bad, f)
			continue
		}
		if !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("disse kan ikke læses som IP-adresser eller områder: %s. Skriv fx 203.0.113.7 eller 192.168.1.0/24", strings.Join(bad, ", "))
	}
	if len(out) > maxAllowIPs {
		return nil, fmt.Errorf("højst %d adresser og områder pr. app (der står %d)", maxAllowIPs, len(out))
	}
	return out, nil
}

// normAllowIP normaliserer én adresse eller ét område. Et område, der dækker alle adresser
// (/0), afvises: det svarer til ingen begrænsning, og feltet skal i stedet stå tomt.
func normAllowIP(s string) (string, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return "", err
		}
		if p.Addr().Is4In6() {
			bits := p.Bits() - 96
			if bits < 0 {
				return "", fmt.Errorf("ugyldigt område")
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), bits)
		}
		if p.Bits() == 0 {
			return "", fmt.Errorf("området dækker alle adresser")
		}
		return p.Masked().String(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", err
	}
	return a.Unmap().WithZone("").String(), nil
}

// ipAllowed afgør, om ip må åbne en app med listen allow. En tom liste tillader alle.
func ipAllowed(allow []string, ip string) bool {
	if len(allow) == 0 {
		return true
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, e := range allow {
		if strings.Contains(e, "/") {
			if p, err := netip.ParsePrefix(e); err == nil && p.Contains(a) {
				return true
			}
		} else if b, err := netip.ParseAddr(e); err == nil && b == a {
			return true
		}
	}
	return false
}

// SetAllowIPs gemmer en apps IP-liste (allerede læst med parseAllowIPs). Tom liste fjerner begrænsningen.
func (m *Manager) SetAllowIPs(slug string, allow []string) error {
	return m.st.Do(func(db *DB) error {
		a, ok := db.Apps[slug]
		if !ok {
			return fmt.Errorf("appen findes ikke")
		}
		a.AllowIPs, a.Updated = allow, time.Now()
		return nil
	})
}
