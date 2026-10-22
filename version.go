package main

import (
	"strconv"
	"strings"
)

// version er Vibe Deploys version. Release-workflowet sætter den ved bygning:
//
//	go build -ldflags "-X main.version=v0.3.0"
//
// En lokal bygning hedder «dev» og opdaterer sig aldrig selv.
var version = "dev"

// semver er en version som v1.2.3. Andre former (fx «dev» eller v1.2.3-rc1) kan ikke sammenlignes.
type semver [3]int

// parseSemver læser «v1.2.3» eller «1.2.3». ok er false for alt andet.
func parseSemver(s string) (v semver, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newer er sand, når candidate er en gyldig version, der er nyere end current.
// En udviklingsudgave (current kan ikke læses) regnes aldrig som forældet.
func newer(candidate, current string) bool {
	c, ok1 := parseSemver(candidate)
	cur, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if c[i] != cur[i] {
			return c[i] > cur[i]
		}
	}
	return false
}
