package main

import (
	"strings"
	"testing"
)

func TestInstallRender(t *testing.T) {
	c := installConfig{Domain: "https://Min-Server.dk/", Email: "a@b.dk", SetupCode: "en-lang-kode-123"}
	if err := c.validate(); err != nil || c.Domain != "min-server.dk" {
		t.Fatalf("domænet skal renses: %q %v", c.Domain, err)
	}
	svc, caddy := renderService(c), renderCaddyfile(c)
	for _, s := range []string{svc, caddy} {
		if strings.Contains(s, "example.com") || strings.Contains(s, "Erstat") {
			t.Fatalf("pladsholdere skal være erstattet:\n%s", s)
		}
	}
	if !strings.Contains(svc, "-domain min-server.dk") || !strings.Contains(svc, "LoadCredential=secret.key:/etc/vibedeploy/secret.key") {
		t.Fatal(svc)
	}
	if !strings.Contains(caddy, "email a@b.dk") || !strings.Contains(caddy, "*.min-server.dk {") || !strings.Contains(caddy, ":80 {") {
		t.Fatal(caddy)
	}
	for _, bad := range []installConfig{
		{Domain: "example.com", Email: "a@b.dk", SetupCode: "123456789012"},
		{Domain: "min-server.dk", Email: "ingen", SetupCode: "123456789012"},
		{Domain: "min-server.dk", Email: "nicklas\u200b@live.dk", SetupCode: "123456789012"}, // usynligt tegn
		{Domain: "min-server.dk", Email: "nøgle@live.dk", SetupCode: "123456789012"},
		{Domain: "min server", Email: "a@b.dk", SetupCode: "123456789012"},
		{Domain: "min-server.dk", Email: "a@b.dk", SetupCode: "kort"},
		{Domain: "min-server.dk", Email: "a@b.dk", SetupCode: "vælg-en-lang-kode-selv"},
		{Domain: "dit-domæne.dk", Email: "a@b.dk", SetupCode: "123456789012"},
	} {
		if bad.validate() == nil {
			t.Errorf("burde afvises: %+v", bad)
		}
	}
}
