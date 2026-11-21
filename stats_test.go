package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testStats laver en Stats i en midlertidig mappe med et ur, testen styrer.
func testStats(t *testing.T, dir string, now *time.Time) *Stats {
	t.Helper()
	cph, _ := time.LoadLocation("Europe/Copenhagen")
	s := NewStats(dir, func() *time.Location { return cph })
	s.now = func() time.Time { return *now }
	return s
}

// browser laver en forespørgsel, som en browser sender den, når en side åbnes.
func browser(path string) *http.Request {
	r := httptest.NewRequest("GET", "http://blog.min-server.dk"+path, nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Safari/605")
	r.Header.Set("Sec-Fetch-Dest", "document")
	return r
}

// TestStatsHits: sidevisninger tæller kun HTML-sider fra browsere, besøgende tælles én gang pr. dag,
// og fejl tælles for sig. En ny dag starter forfra med besøgende, men ugen lægges sammen.
func TestStatsHits(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s := testStats(t, t.TempDir(), &now)

	s.Hit("blog", browser("/"), "203.0.113.1", 200, true)
	s.Hit("blog", browser("/om"), "203.0.113.1", 200, true) // samme person
	img := browser("/logo.png")
	img.Header.Set("Sec-Fetch-Dest", "image")
	s.Hit("blog", img, "203.0.113.1", 200, false)
	s.Hit("blog", browser("/"), "203.0.113.2", 200, true) // en ny person
	bot := browser("/")
	bot.Header.Set("User-Agent", "Googlebot/2.1")
	s.Hit("blog", bot, "66.249.1.1", 200, true)
	pre := browser("/")
	pre.Header.Set("Sec-Purpose", "prefetch")
	s.Hit("blog", pre, "203.0.113.3", 200, true)
	post := browser("/form")
	post.Method = "POST"
	s.Hit("blog", post, "203.0.113.1", 303, false)
	s.Hit("blog", browser("/fejl"), "203.0.113.4", 502, false)

	v := s.AppView(App{Slug: "blog"})
	today := v.Periods[0]
	if today.Views != "3" || today.Visitors != "3" || today.Requests != "8" || today.Errors != "1" {
		t.Fatalf("i dag: %+v", today)
	}
	if v.TodayViews != 3 {
		t.Fatalf("TodayViews %d", v.TodayViews)
	}

	now = now.Add(24 * time.Hour)
	s.Hit("blog", browser("/"), "203.0.113.1", 200, true) // samme person, ny dag: tælles igen
	v = s.AppView(App{Slug: "blog"})
	if v.Periods[0].Visitors != "1" || v.Periods[1].Visitors != "4" || v.Periods[1].Views != "4" {
		t.Fatalf("ny dag: %+v", v.Periods)
	}
	if sum := s.Summary()["blog"]; sum.Today != 1 || sum.Week != 4 {
		t.Fatalf("Summary %+v", sum)
	}
	if !strings.Contains(string(v.ViewsDays.SVG), "<svg") || !strings.Contains(string(v.ViewsHours.SVG), "<svg") {
		t.Fatal("graferne blev ikke tegnet")
	}
}

// TestStatsDayInOwnersZone: dage regnes i ejerens tidszone, ikke i UTC.
func TestStatsDayInOwnersZone(t *testing.T) {
	now := time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC) // 23:30 i København
	s := testStats(t, t.TempDir(), &now)
	s.Hit("blog", browser("/"), "1.1.1.1", 200, true)
	now = now.Add(time.Hour) // 00:30 i København, stadig 21. november i UTC
	s.Hit("blog", browser("/"), "1.1.1.1", 200, true)
	if v := s.AppView(App{Slug: "blog"}); v.Periods[0].Views != "1" || v.Periods[1].Views != "2" {
		t.Fatalf("dagen skiftede ikke ved midnat i København: %+v", v.Periods)
	}
}

// TestStatsPersistAndForget: tallene overlever en genstart, og apps, der er slettet, glemmes ved næste gemning.
func TestStatsPersistAndForget(t *testing.T) {
	m := newTestManager(t)
	_ = m.st.Do(func(db *DB) error {
		db.Apps["blog"] = &App{Slug: "blog", Name: "Blog"}
		return nil
	})
	now := time.Now()
	s := testStats(t, m.dataDir, &now)
	s.Hit("blog", browser("/"), "1.1.1.1", 200, true)
	s.Hit("slettet", browser("/"), "1.1.1.1", 200, true)
	s.save(m)

	s2 := testStats(t, m.dataDir, &now)
	if v := s2.AppView(App{Slug: "blog"}); v.Periods[0].Views != "1" {
		t.Fatalf("efter genstart: %+v", v.Periods[0])
	}
	if _, ok := s2.f.Apps["slettet"]; ok {
		t.Fatal("en slettet app blev husket")
	}
	// Samme besøgende samme dag tælles ikke igen efter en genstart.
	s2.Hit("blog", browser("/"), "1.1.1.1", 200, true)
	if v := s2.AppView(App{Slug: "blog"}); v.Periods[0].Visitors != "1" {
		t.Fatalf("besøgende efter genstart: %s", v.Periods[0].Visitors)
	}
}

// TestStatsObserve: appen regnes først for nede efter to fejlede tjek i træk, nedbruddet lukkes, når den
// svarer igen, og der skrives kun i loggen ved skiftene.
func TestStatsObserve(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s := testStats(t, t.TempDir(), &now)
	live := App{Slug: "blog", Status: StatusLive, Current: 1, HostPort: 1}

	if msg := s.observe("blog", checkResult{OK: true, Ms: 20}); msg != "" {
		t.Fatalf("besked ved et almindeligt tjek: %q", msg)
	}
	now = now.Add(time.Minute)
	if msg := s.observe("blog", checkResult{Err: "svarer ikke"}); msg != "" || s.IsDown("blog") {
		t.Fatalf("ét fejlet tjek må ikke gøre appen nede (%q)", msg)
	}
	now = now.Add(time.Minute)
	if msg := s.observe("blog", checkResult{Err: "svarer ikke"}); !strings.Contains(msg, "svarer ikke") || !s.IsDown("blog") {
		t.Fatalf("to fejlede tjek: %q, nede=%v", msg, s.IsDown("blog"))
	}
	v := s.AppView(live)
	if v.Health.Class != "bad" || len(v.Health.Incidents) != 1 || !strings.Contains(v.Health.Incidents[0].Duration, "stadig nede") {
		t.Fatalf("mens den er nede: %+v", v.Health)
	}
	now = now.Add(3 * time.Minute)
	if msg := s.observe("blog", checkResult{OK: true, Ms: 30}); !strings.Contains(msg, "svarer igen efter 4 min.") {
		t.Fatalf("op igen: %q", msg)
	}
	v = s.AppView(live)
	if v.Health.Class != "ok" || v.Health.Incidents[0].Duration != "4 min." {
		t.Fatalf("efter nedbruddet: %+v", v.Health)
	}
	if got := v.Health.Uptime[0].Pct; got != "50,0 %" { // 2 af 4 tjek svarede
		t.Fatalf("oppetid %q", got)
	}
	if v.Health.AvgMs != "25 ms" || v.Health.MaxMs != "30 ms" {
		t.Fatalf("svartid %q/%q", v.Health.AvgMs, v.Health.MaxMs)
	}

	// En app, der sættes på pause, mens den er nede, står ikke som nede bagefter.
	s.observe("blog", checkResult{Err: "x"})
	s.observe("blog", checkResult{Err: "x"})
	s.unwatched("blog")
	if s.IsDown("blog") {
		t.Fatal("appen står stadig som nede efter pause")
	}
	if v := s.AppView(App{Slug: "blog", Status: StatusStopped}); v.Health.Class != "idle" || v.Health.Incidents[0].Duration == "" {
		t.Fatalf("på pause: %+v", v.Health)
	}
}

// TestStatsCheckAllDemo: overvågningen tjekker en rigtig (demo-)app, skriver i loggen, når den går ned, og
// lader en app på pause være.
func TestStatsCheckAllDemo(t *testing.T) {
	m := newTestManager(t)
	res, err := m.Deploy("Blog", 8080, map[string]string{"index.html": "<h1>hej</h1>"}, "første", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Approve(res.App.Slug); err != nil {
		t.Fatal(err)
	}
	a := waitStatus(t, m, res.App.Slug, StatusLive)
	now := time.Now()
	s := testStats(t, m.dataDir, &now)

	s.checkAll(m)
	if st := s.f.Apps[a.Slug]; st == nil || !st.LastOK {
		t.Fatalf("den kørende app svarede ikke: %+v", st)
	}

	// Containeren forsvinder, men appen står stadig som live: to tjek senere er den nede.
	_ = m.rt.Stop(instName(a.Slug, a.Slot))
	s.checkAll(m)
	s.checkAll(m)
	if !s.IsDown(a.Slug) {
		t.Fatal("appen blev ikke opdaget som nede")
	}
	if log := m.Logs(a.Slug, 50); !strings.Contains(log, "Overvågning: appen") {
		t.Fatalf("intet i loggen: %s", log)
	}

	if err := m.Stop(a.Slug); err != nil {
		t.Fatal(err)
	}
	s.checkAll(m)
	if s.IsDown(a.Slug) {
		t.Fatal("en app på pause må ikke stå som nede")
	}
}

// TestProxyCountsVisits: besøg gennem proxyen tælles med det svar, appen gav, også når appen ikke kører,
// og streaming (Flush) virker stadig gennem statusRecorder.
func TestProxyCountsVisits(t *testing.T) {
	m := newTestManager(t)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<h1>hej</h1>")
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: 1\n\n")
			http.NewResponseController(w).Flush()
		default:
			http.Error(w, "fejl", 500)
		}
	}))
	defer be.Close()
	u, _ := url.Parse(be.URL)
	port, _ := strconv.Atoi(u.Port())
	_ = m.st.Do(func(db *DB) error {
		db.Apps["blog"] = &App{Slug: "blog", Name: "Blog", OwnerID: "o", Visibility: VisPublic, Status: StatusLive, Current: 1, HostPort: port}
		return nil
	})
	now := time.Now()
	s := &Server{st: m.st, mgr: m, domain: "min-server.dk", stats: testStats(t, m.dataDir, &now)}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.proxyApp(rec, browser(path), "blog")
		return rec
	}
	if rec := get("/"); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if rec := get("/stream"); rec.Code != 200 || !rec.Flushed {
		t.Fatalf("streaming: status %d, flushed %v", rec.Code, rec.Flushed)
	}
	get("/api")
	m.update("blog", func(a *App) { a.Status, a.Current = StatusStopped, 0 })
	if rec := get("/"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("på pause: status %d", rec.Code)
	}
	p := s.stats.AppView(App{Slug: "blog"}).Periods[0]
	if p.Views != "1" || p.Requests != "4" || p.Errors != "2" {
		t.Fatalf("tællingen: %+v", p)
	}
}

// TestStatusRecorderHijack: WebSocket-forbindelser (Hijack) når igennem statusRecorder.
func TestStatusRecorderHijack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		conn, _, err := http.NewResponseController(rec).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n\r\n")
		conn.Close()
	}))
	defer srv.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 101") {
		t.Fatalf("svar %q", b)
	}
}

// TestBarChartAndNumbers: tom graf viser en tekst, tal vises på dansk.
func TestBarChartAndNumbers(t *testing.T) {
	if got := string(barChart("x", []bar{{V: 0}, {V: 0}}, func(float64) string { return "" }, "Ingen endnu.")); !strings.Contains(got, "Ingen endnu.") {
		t.Fatalf("tom graf: %s", got)
	}
	if got := string(barChart("x", []bar{{V: 0, Bad: true, Title: "<nede>"}}, func(float64) string { return "" }, "tom")); !strings.Contains(got, "bar bad") || strings.Contains(got, "<nede>") {
		t.Fatalf("rød søjle: %s", got)
	}
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1.000", 1234567: "1.234.567"} {
		if got := numText(in); got != want {
			t.Errorf("numText(%d) = %q", in, got)
		}
	}
	if niceMax(7) != 10 || niceMax(13) != 20 || niceMax(450) != 500 || niceMax(0) != 1 {
		t.Error("niceMax")
	}
	if msText(1234) != "1,2 sek." || msText(35) != "35 ms" {
		t.Error("msText")
	}
}

// TestNilStats: en Server uden Stats (som i de fleste tests) virker stadig.
func TestNilStats(t *testing.T) {
	var s *Stats
	s.Hit("x", browser("/"), "1.1.1.1", 200, true)
	if s.IsDown("x") || len(s.Summary()) != 0 || s.AppView(App{}).Periods != nil {
		t.Fatal("nil Stats skal ikke gøre noget")
	}
	s.save(nil)
}

// TestStatsStatusText: get_status fortæller AI'en, at appen er nede, og hvor mange besøg den har.
func TestStatsStatusText(t *testing.T) {
	now := time.Now()
	s := testStats(t, t.TempDir(), &now)
	s.Hit("blog", browser("/"), "1.1.1.1", 200, true)
	s.observe("blog", checkResult{Err: "svarer ikke"})
	s.observe("blog", checkResult{Err: "svarer ikke"})
	got := s.statusText(App{Slug: "blog", Status: StatusLive, Current: 1, HostPort: 1})
	if !strings.Contains(got, "svarer ikke") || !strings.Contains(got, "1 sidevisninger i dag") {
		t.Fatalf("statusText: %q", got)
	}
}
