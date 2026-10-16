package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestAllPagesRender fanger fejl i skabelonerne, som først viser sig, når siden vises.
func TestAllPagesRender(t *testing.T) {
	pages := loadPages()
	owner := &User{ID: "a", Name: "Anna", Role: RoleOwner, Builder: true}
	app := AppView{App: App{Slug: "x", Name: "X", Status: StatusLive, Current: 1, Visibility: VisShared, FromImage: "nginx:latest",
		Secrets: []KV{{"A", ""}, {"B", "v"}}, Env: []KV{{"K", "v"}}, NeedsRestart: true}, URL: "http://x.localhost", Missing: []string{"A"}}
	cases := map[string]map[string]any{
		"home.html":    {"Apps": []AppView{app}, "Summary": "1 app er live", "CanBuild": true},
		"connect.html": {"MCPURL": "http://localhost/mcp", "NewToken": "vdt_x", "Local": true},
		"consent.html": {"Client": "Claude", "P": authzParams{"c", "r", "s", "ch"}},
		"signup.html":  {"NeedCode": true, "Err": "fejl"},
		"login.html":   {"Next": "/"},
		"invite.html":  {"Err": "fejl"},
		"message.html": {"Text": "hej", "Back": "/"},
		"people.html": {"People": []struct {
			*User
			Apps int
		}{{owner, 2}}, "NewLink": "http://l/invite/x"},
	}
	cases["server.html"] = map[string]any{"Tiles": tiles(HostStats{CPUOK: true, CPU: 40, Cores: 2, MemOK: true, MemUsed: 3 << 30, MemTotal: 4 << 30}),
		"Charts":   []chart{{"CPU", lineChart("CPU", []Sample{{T: time.Now().Add(-time.Minute), CPU: 10}, {T: time.Now(), CPU: 50}}, func(x Sample) float64 { return x.CPU })}},
		"Measured": true, "Uptime": "2 t. 3 min.", "Since": "for 5 min. siden", "Docker": true,
		"Rows":  []UsageRow{{Name: "Blog", Slug: "blog", Status: "Live", Class: "ok", CPU: "1 %", Mem: "20 MB af 512 MB"}},
		"Parts": []struct{ Name, Size, Hint string }{{"Backups", "1 MB", "x"}}}
	cases["setup.html"] = map[string]any{"Checks": []SetupCheck{{Name: "DNS", OK: false, Detail: "x"}, {Name: "Vibe Deploy kører", OK: true}}, "Domain": "d.dk", "IP": "1.2.3.4", "Signup": "https://d.dk/signup"}
	cases["home.html"]["Alert"] = "Disken er 95 % fuld."
	for _, tab := range []string{"oversigt", "versioner", "data", "log", "indstillinger"} {
		cases["app.html#"+tab] = map[string]any{"A": app, "Tab": tab, "Log": "linje",
			"Versions": []struct {
				Version
				Ago              string
				Current, CanBack bool
			}{{Version{N: 1, Pruned: true}, "nu", true, false}},
			"Backups": []BackupInfo{{Name: "20260101-120000.tar.gz", Size: "1 KB", Ago: "nu", Time: time.Now()}},
			"People": []struct {
				ID, Name string
				Checked  bool
			}{{"m", "Mette", true}}, "EnvRows": app.Env, "Prompt": "p", "Usage": map[string]string{"CPU": "1 %", "Mem": "20 MB", "Data": "1 KB", "Backups": "0 B"}}
	}
	for name, data := range cases {
		file := strings.Split(name, "#")[0]
		var buf bytes.Buffer
		if err := pages[file].ExecuteTemplate(&buf, "layout", Page{Title: "t", User: owner, Data: data}); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(buf.String(), "</html>") {
			t.Errorf("%s: siden blev ikke færdig", name)
		}
	}
}
