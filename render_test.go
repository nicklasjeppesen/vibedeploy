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
		"reset.html":   {"Name": "Mette", "Err": "fejl"},
		"invite.html":  {"Err": "fejl"},
		"message.html": {"Text": "hej", "Back": "/"},
		"gitnew.html":  {"F": gitNewForm{Port: "8080", Err: "fejl", Key: &ErrNeedKey{Public: "ssh-ed25519 AAA", AddURL: "https://x", Repo: "h/p", NewKey: true}}},
		"people.html": {"People": []struct {
			*User
			Apps int
		}{{owner, 2}}, "NewLink": "http://l/invite/x", "ResetLink": "http://l/reset/x", "ResetName": "Mette"},
	}
	cases["server.html"] = map[string]any{"Tiles": tiles(HostStats{CPUOK: true, CPU: 40, Cores: 2, MemOK: true, MemUsed: 3 << 30, MemTotal: 4 << 30}),
		"Charts":   []chart{{"CPU", lineChart("CPU", []Sample{{T: time.Now().Add(-time.Minute), CPU: 10}, {T: time.Now(), CPU: 50}}, func(x Sample) float64 { return x.CPU })}},
		"Measured": true, "Uptime": "2 t. 3 min.", "Since": "for 5 min. siden", "Docker": true,
		"Rows":  []UsageRow{{Name: "Blog", Slug: "blog", Status: "Live", Class: "ok", CPU: "1 %", Mem: "20 MB af 512 MB"}},
		"Parts": []struct{ Name, Size, Hint string }{{"Backups", "1 MB", "x"}},
		"Self": map[string]any{"Version": "v0.2.0", "Release": true, "Installed": true, "HasKey": true, "CanUpdate": true,
			"Latest": "v0.3.0", "Notes": "https://x", "Checked": "nu", "CheckErr": "timeout", "Auto": true,
			"Status": UpdateStatus{State: "rolledback", Message: "v0.3.0 svarede ikke"}, "StatusAgo": "i går",
			"Days": []struct {
				Key, Label string
				Selected   bool
			}{{"daily", "Hver dag", false}, {"sun", "Søndag", true}}, "At": "03:00", "Schedule": "hver søndag kl. 03:00", "Next": "søndag 25. oktober kl. 03:00",
			"TZ": "Europe/Copenhagen", "TZLabel": "Danmark (København)", "TZOther": false,
			"TZs": []struct {
				Zone, Label string
				Selected    bool
			}{{"Europe/Copenhagen", "Danmark (København) · UTC+1", true}, {"UTC", "UTC (verdenstid) · UTC", false}}}}
	cases["server.html"]["Cache"] = cacheView{Docker: true, BuildCache: "3.2GB", Reclaim: "3.2GB", Orphans: 2, OrphanSize: "240 MB",
		Last: "for 2 min. siden", LastBy: "Anna", LastFreed: "1,1 GB", LastDone: []string{"Byggecachen er ryddet (1,1 GB)."}, LastErr: "x"}
	cases["server.html#dev"] = map[string]any{"Self": map[string]any{"Version": "dev"}, "Cache": cacheView{Running: true}}
	cases["home.html"]["Down"] = []string{"X"}

	// Besøg og overvågning med rigtige tal: en app, der har været nede og er oppe igen.
	now := time.Now()
	st := NewStats(t.TempDir(), nil)
	st.now = func() time.Time { return now }
	st.Hit("x", browser("/"), "1.1.1.1", 200, true)
	st.observe("x", checkResult{Err: "svarer ikke"})
	st.observe("x", checkResult{Err: "svarer ikke"})
	st.observe("x", checkResult{OK: true, Ms: 12})
	statsLive, statsDown := st.AppView(app.App), st.AppView(app.App)
	st.observe("x", checkResult{Err: "svarer ikke"})
	st.observe("x", checkResult{Err: "svarer ikke"})
	statsDown = st.AppView(app.App)
	cases["setup.html"] = map[string]any{"Checks": []SetupCheck{{Name: "DNS", OK: false, Detail: "x"}, {Name: "Vibe Deploy kører", OK: true}}, "Domain": "d.dk", "IP": "1.2.3.4", "Signup": "https://d.dk/signup"}
	cases["home.html"]["Alert"] = "Disken er 95 % fuld."
	for _, tab := range []string{"oversigt", "versioner", "data", "log", "indstillinger", "statistik", "statistik#nede", "statistik#tom", "oversigt#tom"} {
		stats := statsLive
		switch tab {
		case "statistik#nede":
			stats = statsDown
		case "statistik#tom", "oversigt#tom":
			stats = (*Stats)(nil).AppView(app.App) // en Server uden Stats
		}
		cases["app.html#"+tab] = map[string]any{"A": app, "Tab": strings.Split(tab, "#")[0], "Log": "linje", "Stats": stats,
			"Versions": []struct {
				Version
				Ago              string
				Current, CanBack bool
			}{{Version{N: 1, Pruned: true}, "nu", true, false}},
			"Backups": []BackupInfo{{Name: "20260101-120000.tar.gz", Size: "1 KB", Ago: "nu", Time: time.Now()}},
			"People": []struct {
				ID, Name string
				Checked  bool
			}{{"m", "Mette", true}}, "EnvRows": app.Env, "AllowIPsText": "203.0.113.7\n192.168.1.0/24", "YourIP": "203.0.113.7", "Prompt": "p", "Usage": map[string]string{"CPU": "1 %", "Mem": "20 MB", "Data": "1 KB", "Backups": "0 B"}}
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
