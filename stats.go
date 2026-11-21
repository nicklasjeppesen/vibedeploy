package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- Besøgstal og overvågning pr. app ----------
//
// Stats tæller trafikken, der går gennem proxyen til hver app (proxyApp kalder Hit), og tjekker hvert
// minut, om hver kørende app svarer (Run → check). Alt samles i spande pr. time (de seneste 48 timer) og
// pr. dag (de seneste 90 dage, i ejerens valgte tidszone) og gemmes i data/stats.json, så tallene overlever
// en genstart. Filen er ikke en del af state.db, fordi den ændrer sig ved hvert besøg; en ældre version af
// Vibe Deploy ignorerer den bare.
//
// Besøgende tælles uden at gemme IP-adresser: IP og browser hashes med et tilfældigt salt, der skiftes hver
// dag, og kun de første 8 bytes af hashet huskes, og kun for i dag. Samme person kan derfor ikke genkendes
// fra dag til dag, og antallet af besøgende for en uge er summen af dagene.

// statBucket er tallene for ét tidsrum (én time eller én dag).
type statBucket struct {
	Start    time.Time
	Requests int   `json:",omitempty"` // alle forespørgsler gennem proxyen
	Views    int   `json:",omitempty"` // sidevisninger: HTML-sider, der blev vist (ikke billeder, scripts, API-kald, robotter)
	Errors   int   `json:",omitempty"` // svar 500 og derover (også når appen ikke svarer)
	Visitors int   `json:",omitempty"` // forskellige besøgende (kun i dage)
	Checks   int   `json:",omitempty"` // overvågningens tjek
	Up       int   `json:",omitempty"` // tjek, hvor appen svarede
	MsSum    int64 `json:",omitempty"` // samlet svartid for de vellykkede tjek (til gennemsnit)
	MsMax    int64 `json:",omitempty"` // langsomste vellykkede tjek
}

// Incident er en periode, hvor appen ikke svarede. End er nul, mens den stadig er nede.
type Incident struct {
	Start, End time.Time
	Reason     string
}

// appStats er alt, Stats ved om én app.
type appStats struct {
	Hours     []statBucket
	Days      []statBucket
	Seen      []uint64   `json:",omitempty"` // hashes af dagens besøgende (se visitorHash)
	SeenDay   time.Time  `json:",omitzero"`  // dagen, Seen gælder for
	Down      bool       `json:",omitempty"` // svarer ikke (mindst downAfter tjek i træk)
	Fails     int        `json:",omitempty"` // fejlede tjek i træk
	FailSince time.Time  `json:",omitzero"`  // første fejlede tjek i træk
	LastCheck time.Time  `json:",omitzero"`
	LastOK    bool       `json:",omitempty"`
	LastMs    int64      `json:",omitempty"`
	LastErr   string     `json:",omitempty"`
	Incidents []Incident `json:",omitempty"` // de nyeste maxIncidents
	seen      map[uint64]struct{}
}

// statsFile er formen på data/stats.json.
type statsFile struct {
	Salt    string    // salt til dagens hashes af besøgende; skiftes hver dag
	SaltDay time.Time // dagen, saltet gælder for
	Apps    map[string]*appStats
}

const (
	statsFileName = "stats.json"
	keepHours     = 48 * time.Hour
	keepDays      = 90               // dage
	maxSeen       = 20000            // højst så mange besøgende pr. app pr. dag huskes; resten tælles ikke som nye
	maxIncidents  = 20               // nedbrud, der huskes pr. app
	probeEvery    = time.Minute      // hvor tit hver app tjekkes
	checkTimeout  = 10 * time.Second // hvor længe et tjek venter på svar
	downAfter     = 2                // så mange fejlede tjek i træk, før appen regnes for nede
	statsSaveEach = time.Minute      // hvor tit stats.json skrives, hvis noget er ændret
)

// Stats holder besøgstal og overvågning for alle apps. Alle metoder virker også på en nil *Stats (gør intet),
// så tests og værktøjer, der laver en Server uden Stats, ikke skal tage stilling til den.
type Stats struct {
	path   string
	tz     func() *time.Location // ejerens tidszone (Settings.UpdateTZ); dage regnes i den
	now    func() time.Time
	client *http.Client

	mu    sync.Mutex
	f     statsFile
	dirty bool
}

// NewStats indlæser dir/stats.json (eller starter forfra, hvis filen mangler eller ikke kan læses).
func NewStats(dir string, tz func() *time.Location) *Stats {
	s := &Stats{path: filepath.Join(dir, statsFileName), tz: tz, now: time.Now,
		client: &http.Client{Timeout: checkTimeout, Transport: appTransport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if b, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(b, &s.f); err != nil {
			fmt.Printf("stats.json kunne ikke læses, besøgstallene starter forfra: %v\n", err)
			s.f = statsFile{}
		}
	}
	if s.f.Apps == nil {
		s.f.Apps = map[string]*appStats{}
	}
	for _, a := range s.f.Apps {
		a.seen = make(map[uint64]struct{}, len(a.Seen))
		for _, h := range a.Seen {
			a.seen[h] = struct{}{}
		}
	}
	return s
}

// loc er tidszonen, dage regnes i.
func (s *Stats) loc() *time.Location {
	if s.tz != nil {
		if l := s.tz(); l != nil {
			return l
		}
	}
	return time.UTC
}

// dayStart er midnat i tidszonen loc for dagen, t ligger i.
func dayStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// app returnerer appens tal og opretter dem, hvis de ikke findes. Kaldes med s.mu låst.
func (s *Stats) app(slug string) *appStats {
	a := s.f.Apps[slug]
	if a == nil {
		a = &appStats{seen: map[uint64]struct{}{}}
		s.f.Apps[slug] = a
	}
	return a
}

// bump lægger til i spanden, der starter ved start, og fjerner spande, der er ældre end cut.
// Spandene ligger i rækkefølge; går uret baglæns, lægges der til i den nyeste.
func bump(list []statBucket, start, cut time.Time, f func(b *statBucket)) []statBucket {
	n := len(list)
	switch {
	case n > 0 && list[n-1].Start.Equal(start):
	case n > 0 && start.Before(list[n-1].Start):
		start = list[n-1].Start
	default:
		list = append(list, statBucket{Start: start})
	}
	f(&list[len(list)-1])
	i := 0
	for i < len(list)-1 && list[i].Start.Before(cut) {
		i++
	}
	return list[i:]
}

// record lægger f til i appens time- og dagsspand for tidspunktet now. Kaldes med s.mu låst.
func (s *Stats) record(a *appStats, now time.Time, f func(b *statBucket)) {
	loc := s.loc()
	a.Hours = bump(a.Hours, now.Truncate(time.Hour), now.Add(-keepHours), f)
	day := dayStart(now, loc)
	a.Days = bump(a.Days, day, day.AddDate(0, 0, -keepDays), f)
	s.dirty = true
}

// botRe genkender robotter, overvågning og forhåndsvisninger af links (Slack, Messenger …) på deres User-Agent.
var botRe = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|preview|monitor|uptime|headless|lighthouse|curl/|wget|python-|go-http-client|okhttp|java/|axios|node-fetch|facebookexternalhit|whatsapp`)

// isBot er sand for forespørgsler fra robotter og værktøjer (også uden User-Agent).
func isBot(r *http.Request) bool {
	ua := r.UserAgent()
	return ua == "" || botRe.MatchString(ua)
}

// isPageView afgør, om en forespørgsel var en sidevisning: en GET, der gav en HTML-side, som browseren
// selv bad om at vise (ikke en forhåndshentning, et billede, et script eller et API-kald).
func isPageView(r *http.Request, status int, html bool) bool {
	if r.Method != http.MethodGet || status < 200 || status >= 400 || !html {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return false
	}
	p := r.Header.Get("Sec-Purpose") + r.Header.Get("Purpose")
	return !strings.Contains(p, "prefetch") && !strings.Contains(p, "prerender")
}

// visitorHash er et kort, saltet hash af IP-adresse og browser. Saltet skiftes hver dag, så hashet ikke
// kan bruges til at genkende nogen senere, og IP-adressen gemmes aldrig.
func visitorHash(salt, ip, ua string) uint64 {
	h := sha256.Sum256([]byte(salt + "\x00" + ip + "\x00" + ua))
	return binary.BigEndian.Uint64(h[:8])
}

// Hit tæller én forespørgsel til appen slug. status er svaret, html om svaret var en HTML-side.
func (s *Stats) Hit(slug string, r *http.Request, ip string, status int, html bool) {
	if s == nil {
		return
	}
	bot := isBot(r)
	view := !bot && isPageView(r, status, html)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	day := dayStart(now, s.loc())
	if s.f.Salt == "" || !s.f.SaltDay.Equal(day) {
		s.f.Salt, s.f.SaltDay = newSalt(), day
	}
	a := s.app(slug)
	newVisitor := false
	if !bot && status < 400 {
		if !a.SeenDay.Equal(day) {
			a.Seen, a.SeenDay, a.seen = nil, day, map[uint64]struct{}{}
		}
		h := visitorHash(s.f.Salt, ip, r.UserAgent())
		if _, ok := a.seen[h]; !ok && len(a.seen) < maxSeen {
			a.seen[h] = struct{}{}
			a.Seen = append(a.Seen, h)
			newVisitor = true
		}
	}
	s.record(a, now, func(b *statBucket) {
		b.Requests++
		if status >= 500 {
			b.Errors++
		}
		if view {
			b.Views++
		}
		if newVisitor && b.Start.Equal(day) {
			b.Visitors++
		}
	})
}

// newSalt er 16 tilfældige bytes som hex.
func newSalt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// checkResult er udfaldet af ét tjek.
type checkResult struct {
	OK  bool
	Ms  int64
	Err string
}

// probe spørger appen på 127.0.0.1:port/health, som udrulningen gør (waitHealthy): ethvert svar under 500
// tæller som «svarer», så apps uden en /health-side også kan overvåges.
func (s *Stats) probe(port int) checkResult {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	req.Header.Set("User-Agent", "VibeDeploy-overvaagning")
	resp, err := s.client.Do(req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		reason := "svarer ikke"
		if strings.Contains(err.Error(), "Timeout") || strings.Contains(err.Error(), "deadline") {
			reason = fmt.Sprintf("svarede ikke inden for %d sekunder", int(checkTimeout.Seconds()))
		} else if strings.Contains(err.Error(), "refused") {
			reason = "tager ikke imod forbindelser (er den gået ned?)"
		}
		return checkResult{Ms: ms, Err: reason}
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return checkResult{Ms: ms, Err: fmt.Sprintf("svarede med fejl %d", resp.StatusCode)}
	}
	return checkResult{OK: true, Ms: ms}
}

// observe gemmer resultatet af et tjek og returnerer en besked til appens log, hvis appen lige er gået ned
// eller kommet op igen (ellers tom).
func (s *Stats) observe(slug string, res checkResult) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	a := s.app(slug)
	a.LastCheck, a.LastOK, a.LastMs, a.LastErr = now, res.OK, res.Ms, res.Err
	s.record(a, now, func(b *statBucket) {
		b.Checks++
		if res.OK {
			b.Up++
			b.MsSum += res.Ms
			if res.Ms > b.MsMax {
				b.MsMax = res.Ms
			}
		}
	})
	if res.OK {
		a.Fails, a.FailSince = 0, time.Time{}
		if a.Down {
			a.Down = false
			if n := len(a.Incidents); n > 0 && a.Incidents[n-1].End.IsZero() {
				a.Incidents[n-1].End = now
				return "Overvågning: appen svarer igen efter " + durText(now.Sub(a.Incidents[n-1].Start)) + "."
			}
			return "Overvågning: appen svarer igen."
		}
		return ""
	}
	if a.Fails == 0 {
		a.FailSince = now
	}
	a.Fails++
	if a.Fails >= downAfter && !a.Down {
		a.Down = true
		a.Incidents = append(a.Incidents, Incident{Start: a.FailSince, Reason: res.Err})
		if len(a.Incidents) > maxIncidents {
			a.Incidents = a.Incidents[len(a.Incidents)-maxIncidents:]
		}
		return "Overvågning: appen " + res.Err + ". Den er tjekket " + fmt.Sprint(a.Fails) + " gange i træk uden svar."
	}
	return ""
}

// unwatched markerer, at appen ikke længere kører (pause, fejlet, slettet): et åbent nedbrud lukkes, så appen
// ikke står som «nede», mens den bevidst er stoppet.
func (s *Stats) unwatched(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.Apps[slug]
	if a == nil || (!a.Down && a.Fails == 0) {
		return
	}
	if n := len(a.Incidents); a.Down && n > 0 && a.Incidents[n-1].End.IsZero() {
		a.Incidents[n-1].End = s.now()
	}
	a.Down, a.Fails, a.FailSince = false, 0, time.Time{}
	s.dirty = true
}

// IsDown er sand, når appen ikke har svaret på de seneste tjek.
func (s *Stats) IsDown(slug string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.Apps[slug]
	return a != nil && a.Down
}

// Run tjekker alle kørende apps hvert minut og gemmer stats.json, når noget er ændret. Den stopper, når stop
// lukkes, og gemmer en sidste gang. mgr bruges til at finde appene og skrive i deres log.
func (s *Stats) Run(mgr *Manager, stop <-chan struct{}) {
	if s == nil {
		return
	}
	check := time.NewTicker(probeEvery)
	save := time.NewTicker(statsSaveEach)
	defer check.Stop()
	defer save.Stop()
	for {
		select {
		case <-stop:
			s.save(mgr)
			return
		case <-check.C:
			s.checkAll(mgr)
		case <-save.C:
			s.save(mgr)
		}
	}
}

// checkAll tjekker alle apps, der skal svare lige nu (samtidig), og skriver i appens log, når en app går ned
// eller kommer op igen. Siderne i dashboardet får besked (notify), så de viser det med det samme.
func (s *Stats) checkAll(mgr *Manager) {
	db := mgr.st.Snapshot()
	var wg sync.WaitGroup
	for slug, a := range db.Apps {
		if !serving(*a) || a.HostPort == 0 {
			s.unwatched(slug)
			continue
		}
		wg.Add(1)
		go func(slug string, port int) {
			defer wg.Done()
			msg := s.observe(slug, s.probe(port))
			if msg != "" {
				mgr.logger(slug)("%s", msg)
				mgr.notify(slug)
			}
		}(slug, a.HostPort)
	}
	wg.Wait()
}

// save skriver stats.json, hvis noget er ændret, og glemmer apps, der ikke findes mere.
func (s *Stats) save(mgr *Manager) {
	if s == nil {
		return
	}
	exists := map[string]bool{}
	if mgr != nil {
		mgr.st.View(func(db *DB) {
			for slug := range db.Apps {
				exists[slug] = true
			}
		})
	}
	s.mu.Lock()
	if mgr != nil {
		for slug := range s.f.Apps {
			if !exists[slug] {
				delete(s.f.Apps, slug)
				s.dirty = true
			}
		}
	}
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	b, err := json.Marshal(&s.f)
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(s.path, b, 0o600)
	}
	if err != nil {
		fmt.Printf("stats.json kunne ikke gemmes: %v\n", err)
	}
}

// ---------- visning ----------

// sumSince lægger spandene, der starter ved since eller senere, sammen.
func sumSince(list []statBucket, since time.Time) statBucket {
	var t statBucket
	for _, b := range list {
		if b.Start.Before(since) {
			continue
		}
		t.Requests += b.Requests
		t.Views += b.Views
		t.Errors += b.Errors
		t.Visitors += b.Visitors
		t.Checks += b.Checks
		t.Up += b.Up
		t.MsSum += b.MsSum
		if b.MsMax > t.MsMax {
			t.MsMax = b.MsMax
		}
	}
	return t
}

// TrafficPeriod er besøgstal for et tidsrum, klar til visning.
type TrafficPeriod struct {
	Name                              string
	Views, Visitors, Requests, Errors string
}

// HealthView er overvågningen af én app, klar til visning.
type HealthView struct {
	Label, Class, Since, LastCheck, LastMs, LastErr string
	Uptime                                          []struct{ Name, Pct string }
	AvgMs, MaxMs                                    string
	Incidents                                       []IncidentView
	Watched                                         bool // tjekkes appen (kører den)?
}

// IncidentView er ét nedbrud, klar til visning.
type IncidentView struct{ When, Duration, Reason string }

// AppStatsView er det, appens side viser under «Besøg» og «Overvågning».
type AppStatsView struct {
	Periods     []TrafficPeriod
	TodayViews  int
	ViewsHours  chart
	ViewsDays   chart
	LatencyHour chart
	Health      HealthView
}

// numText viser et heltal med punktum som tusindtalsseparator, fx «12.345».
func numText(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + numText(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	return s
}

// msText viser en svartid, fx «35 ms» eller «1,2 sek.».
func msText(ms int64) string {
	if ms >= 1000 {
		return strings.Replace(fmt.Sprintf("%.1f sek.", float64(ms)/1000), ".", ",", 1)
	}
	return fmt.Sprintf("%d ms", ms)
}

// durText viser en varighed i almindelige ord, fx «4 min.» eller «2 t. 5 min.».
func durText(d time.Duration) string {
	if d < time.Minute {
		return "under et minut"
	}
	return uptimeText(d)
}

// uptimePct viser andelen af vellykkede tjek, fx «99,8 %», eller «–» uden tjek.
func uptimePct(b statBucket) string {
	if b.Checks == 0 {
		return "–"
	}
	p := 100 * float64(b.Up) / float64(b.Checks)
	if p < 100 && p >= 99.95 {
		p = 99.9 // vis aldrig 100 %, hvis appen har været nede
	}
	if p == 100 {
		return "100 %"
	}
	return strings.Replace(fmt.Sprintf("%.1f %%", p), ".", ",", 1)
}

// AppView samler besøgstal og overvågning for appen a til appens side.
func (s *Stats) AppView(a App) AppStatsView {
	var v AppStatsView
	if s == nil {
		return v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now, loc := s.now(), s.loc()
	st := s.f.Apps[a.Slug]
	if st == nil {
		st = &appStats{}
	}
	today := dayStart(now, loc)
	periods := []struct {
		name  string
		since time.Time
	}{{"I dag", today}, {"Seneste 7 dage", today.AddDate(0, 0, -6)}, {"Seneste 30 dage", today.AddDate(0, 0, -29)}}
	for _, p := range periods {
		t := sumSince(st.Days, p.since)
		v.Periods = append(v.Periods, TrafficPeriod{p.name, numText(t.Views), numText(t.Visitors), numText(t.Requests), numText(t.Errors)})
		if p.name == "I dag" {
			v.TodayViews = t.Views
		}
	}

	// Sidevisninger pr. time det seneste døgn og pr. dag de seneste 30 dage.
	hour := now.Truncate(time.Hour)
	var hours, latency []bar
	for i := 23; i >= 0; i-- {
		h := hour.Add(-time.Duration(i) * time.Hour)
		b := sumSince(bucketAt(st.Hours, h), h)
		label := h.In(loc).Format("15:04")
		hours = append(hours, bar{Label: label, V: float64(b.Views), Title: fmt.Sprintf("kl. %s · %s %s", label, numText(b.Views), plural(b.Views, "sidevisning", "sidevisninger"))})
		lb := bar{Label: label, Title: "kl. " + label + " · ikke tjekket"}
		if b.Checks > 0 {
			lb.Bad = b.Up < b.Checks
			if b.Up > 0 {
				lb.V = float64(b.MsSum) / float64(b.Up)
			}
			lb.Title = fmt.Sprintf("kl. %s · gennemsnit %s, %d af %d tjek svarede", label, msText(int64(lb.V)), b.Up, b.Checks)
		}
		latency = append(latency, lb)
	}
	var days []bar
	for i := 29; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		b := sumSince(bucketAt(st.Days, d), d)
		days = append(days, bar{Label: d.Format("2/1"), V: float64(b.Views),
			Title: fmt.Sprintf("%s · %s %s, %s %s", danishDate(d), numText(b.Views), plural(b.Views, "sidevisning", "sidevisninger"), numText(b.Visitors), plural(b.Visitors, "besøgende", "besøgende"))})
	}
	v.ViewsHours = chart{"Sidevisninger pr. time, det seneste døgn", barChart("Sidevisninger pr. time", hours, func(x float64) string { return numText(int(x)) }, "Ingen sidevisninger det seneste døgn.")}
	v.ViewsDays = chart{"Sidevisninger pr. dag, de seneste 30 dage", barChart("Sidevisninger pr. dag", days, func(x float64) string { return numText(int(x)) }, "Ingen sidevisninger de seneste 30 dage.")}
	v.LatencyHour = chart{"Svartid pr. time, det seneste døgn", barChart("Svartid pr. time", latency, func(x float64) string { return msText(int64(x)) }, "Ingen tjek endnu. Det første kommer inden for et minut.")}

	// Overvågning.
	h := HealthView{Watched: serving(a) && a.HostPort > 0}
	switch {
	case !h.Watched:
		h.Label, h.Class = "Overvåges ikke", "idle"
		h.Since = "Appen kører ikke lige nu, så den tjekkes ikke."
	case st.Down:
		h.Label, h.Class = "Svarer ikke", "bad"
		if n := len(st.Incidents); n > 0 {
			h.Since = "Siden " + danishTime(st.Incidents[n-1].Start, loc) + " (" + durText(now.Sub(st.Incidents[n-1].Start)) + ")"
		}
	case st.LastCheck.IsZero():
		h.Label, h.Class = "Venter på første tjek", "idle"
	case !st.LastOK:
		h.Label, h.Class = "Svarede ikke sidst", "warn"
	default:
		h.Label, h.Class = "Svarer", "ok"
	}
	if !st.LastCheck.IsZero() {
		h.LastCheck = relTime(st.LastCheck)
		if st.LastOK {
			h.LastMs = msText(st.LastMs)
		}
		h.LastErr = st.LastErr
	}
	day24 := sumSince(st.Hours, now.Add(-24*time.Hour))
	h.Uptime = []struct{ Name, Pct string }{
		{"seneste døgn", uptimePct(day24)},
		{"seneste 7 dage", uptimePct(sumSince(st.Days, today.AddDate(0, 0, -6)))},
		{"seneste 30 dage", uptimePct(sumSince(st.Days, today.AddDate(0, 0, -29)))},
	}
	if day24.Up > 0 {
		h.AvgMs, h.MaxMs = msText(day24.MsSum/int64(day24.Up)), msText(day24.MsMax)
	}
	for i := len(st.Incidents) - 1; i >= 0; i-- {
		in := st.Incidents[i]
		iv := IncidentView{When: danishTime(in.Start, loc), Reason: in.Reason}
		if in.End.IsZero() {
			iv.Duration = "stadig nede (" + durText(now.Sub(in.Start)) + ")"
		} else {
			iv.Duration = durText(in.End.Sub(in.Start))
		}
		h.Incidents = append(h.Incidents, iv)
	}
	v.Health = h
	return v
}

// bucketAt er spanden, der starter præcis ved start (som en liste med ét element), eller ingen.
func bucketAt(list []statBucket, start time.Time) []statBucket {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Start.Equal(start) {
			return list[i : i+1]
		}
	}
	return nil
}

// danishMonths er månedernes korte navne.
var danishMonths = [...]string{"jan.", "feb.", "mar.", "apr.", "maj", "jun.", "jul.", "aug.", "sep.", "okt.", "nov.", "dec."}

// danishDate viser en dag, fx «3. nov.».
func danishDate(t time.Time) string {
	return fmt.Sprintf("%d. %s", t.Day(), danishMonths[t.Month()-1])
}

// danishTime viser et tidspunkt i loc, fx «3. nov. kl. 14:05».
func danishTime(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	return danishDate(t) + " kl. " + t.Format("15:04")
}

// StatsSummary er de korte tal til forsiden og siden Server: sidevisninger i dag og de seneste 7 dage,
// oppetid det seneste døgn og om appen er nede.
type StatsSummary struct {
	Today, Week int
	Uptime      string
	Down        bool
}

// Summary returnerer de korte tal for alle apps.
func (s *Stats) Summary() map[string]StatsSummary {
	out := map[string]StatsSummary{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now, loc := s.now(), s.loc()
	today := dayStart(now, loc)
	for slug, a := range s.f.Apps {
		out[slug] = StatsSummary{
			Today:  sumSince(a.Days, today).Views,
			Week:   sumSince(a.Days, today.AddDate(0, 0, -6)).Views,
			Uptime: uptimePct(sumSince(a.Hours, now.Add(-24*time.Hour))),
			Down:   a.Down,
		}
	}
	return out
}

// statusText er en kort tekst om overvågning og besøg til MCP-værktøjet get_status (tom uden Stats).
// Den afslører kun tal, ingen oplysninger om de besøgende.
func (s *Stats) statusText(a App) string {
	if s == nil {
		return ""
	}
	v := s.AppView(a)
	out := ""
	if v.Health.Watched {
		out += " Overvågning: " + strings.ToLower(v.Health.Label)
		if v.Health.Class == "bad" {
			out += " (" + v.Health.Since + "; appen " + v.Health.LastErr + ")"
		}
		out += ", oppetid det seneste døgn " + v.Health.Uptime[0].Pct + "."
	}
	if len(v.Periods) > 1 {
		out += fmt.Sprintf(" Besøg: %s sidevisninger i dag, %s de seneste 7 dage; %s fejlsvar (500+) i dag.", v.Periods[0].Views, v.Periods[1].Views, v.Periods[0].Errors)
	}
	return out
}

// downNames er navnene på de apps blandt apps, der ikke svarer, sorteret.
func (s *Stats) downNames(apps []AppView) []string {
	var out []string
	for _, a := range apps {
		if s.IsDown(a.Slug) {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- søjlediagram ----------

// bar er én søjle: værdi, etiket på aksen og tekst, når man holder musen over. Bad farver søjlen rød.
type bar struct {
	Label, Title string
	V            float64
	Bad          bool
}

// niceMax runder op til et pænt tal (1, 2 eller 5 gange en tierpotens), så aksen er let at læse.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := 1.0
	for p*10 <= v {
		p *= 10
	}
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

// barChart tegner søjler som SVG i samme stil som lineChart: en diskret skala, en søjle pr. tidsrum og en
// tekst, når man holder musen over (title). Er alle søjler nul (og ingen røde), vises empty i stedet.
func barChart(name string, bars []bar, axis func(float64) string, empty string) template.HTML {
	maxV, some := 0.0, false
	for _, b := range bars {
		if b.V > maxV {
			maxV = b.V
		}
		some = some || b.V > 0 || b.Bad
	}
	if len(bars) == 0 || !some {
		return template.HTML(`<p class="muted small chart-empty">` + template.HTMLEscapeString(empty) + `</p>`)
	}
	top := niceMax(maxV)
	y := func(v float64) float64 { return padT + plotH*(1-v/top) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s">`, chartW, chartH, template.HTMLEscapeString(name))
	for _, v := range []float64{0, top / 2, top} {
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/><text class="axis" x="%d" y="%.1f" text-anchor="end">%s</text>`,
			padL, chartW-padR, y(v), y(v), padL-8, y(v)+4, template.HTMLEscapeString(axis(v)))
	}
	step := float64(plotW) / float64(len(bars))
	w := step * 0.7
	if w < 1 {
		w = 1
	}
	for i, br := range bars {
		x := padL + step*float64(i) + (step-w)/2
		h := float64(plotH) * br.V / top
		cls := "bar"
		if br.Bad {
			cls += " bad"
			if h < 6 {
				h = 6 // en fejl skal kunne ses, også når der ikke er en svartid
			}
		}
		fmt.Fprintf(&b, `<g class="hit"><rect class="slot" x="%.1f" y="%d" width="%.1f" height="%d"/><rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="1.5"/><title>%s</title></g>`,
			padL+step*float64(i), padT, step, plotH, cls, x, float64(padT+plotH)-h, w, h, template.HTMLEscapeString(br.Title))
	}
	fmt.Fprintf(&b, `<text class="axis" x="%d" y="%d">%s</text><text class="axis" x="%d" y="%d" text-anchor="end">%s</text>`,
		padL, chartH-6, template.HTMLEscapeString(bars[0].Label), chartW-padR, chartH-6, template.HTMLEscapeString(bars[len(bars)-1].Label))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
