package main

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------- siden «Server» (kun ejeren): CPU, hukommelse, disk og hver apps forbrug ----------

// Tile er et nøgletal øverst på siden med en måler og en status i ord.
type Tile struct {
	Name, Value, Detail string
	Pct                 float64 // til måleren, 0–100
	Label, Class        string  // status i ord og farve, fx «Fint»/ok
	OK                  bool    // false, når platformen ikke kan måles
}

// UsageRow er én app i tabellen over forbrug.
type UsageRow struct {
	Name, Slug, Status, Class      string
	CPU, Mem, Data, Backups, Files string
	MemBytes                       uint64
}

// level oversætter en procent til status: under 75 er fint, under 90 er højt, ellers kritisk.
func level(p float64) (label, class string) {
	switch {
	case p >= 90:
		return "Kritisk", "bad"
	case p >= 75:
		return "Højt", "warn"
	}
	return "Fint", "ok"
}

// bytesText viser bytes som fx «1,4 GB».
func bytesText(n uint64) string { return humanSize(int64(n)) }

// pctText viser en procent med højst én decimal, dansk komma.
func pctText(p float64) string {
	if p < 10 {
		return strings.Replace(fmt.Sprintf("%.1f %%", p), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f %%", p)
}

// uptimeText viser oppetid som «3 dage, 4 t.» eller «2 t. 10 min.».
func uptimeText(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	if days > 0 {
		return fmt.Sprintf("%d %s, %d t.", days, plural(days, "dag", "dage"), hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%d t. %d min.", hours, mins)
	}
	return fmt.Sprintf("%d min.", mins)
}

// tiles laver de tre nøgletal ud fra den seneste måling.
func tiles(h HostStats) []Tile {
	cpu := Tile{Name: "CPU", OK: h.CPUOK, Pct: h.CPU, Value: pctText(h.CPU)}
	cpu.Detail = fmt.Sprintf("%d %s", h.Cores, plural(h.Cores, "kerne", "kerner"))
	if h.LoadOK {
		cpu.Detail += strings.Replace(fmt.Sprintf(" · belastning %.2f", h.Load[0]), ".", ",", 1)
	}
	mem := Tile{Name: "Hukommelse", OK: h.MemOK}
	if h.MemOK && h.MemTotal > 0 {
		mem.Pct = 100 * float64(h.MemUsed) / float64(h.MemTotal)
		mem.Value = pctText(mem.Pct)
		mem.Detail = bytesText(h.MemUsed) + " af " + bytesText(h.MemTotal)
	}
	disk := Tile{Name: "Disk", OK: h.DiskOK}
	if h.DiskOK && h.DiskTotal > 0 {
		disk.Pct = 100 * float64(h.DiskUsed) / float64(h.DiskTotal)
		disk.Value = pctText(disk.Pct)
		disk.Detail = bytesText(h.DiskTotal-h.DiskUsed) + " fri af " + bytesText(h.DiskTotal)
	}
	out := []Tile{cpu, mem, disk}
	for i := range out {
		out[i].Label, out[i].Class = level(out[i].Pct)
		if !out[i].OK {
			out[i].Value, out[i].Detail, out[i].Label, out[i].Class = "–", "Kan ikke måles på denne platform", "", "idle"
		}
	}
	return out
}

// serverPage viser serverens ressourcer.
func (s *Server) serverPage(w http.ResponseWriter, r *http.Request, u *User) {
	h, samples, usage, disk, df := s.mon.Snapshot()
	db := s.st.Snapshot()

	var rows []UsageRow
	for slug, a := range db.Apps {
		us := usage[slug]
		row := UsageRow{Name: a.Name, Slug: slug, Status: statusLabel(a.Status), Class: statusClass(a.Status),
			Data: humanSize(us.Data), Backups: humanSize(us.Backups), Files: humanSize(us.Sources), CPU: "–", Mem: "–"}
		if us.Running {
			row.CPU, row.MemBytes = pctText(us.CPU), us.MemUsed
			row.Mem = bytesText(us.MemUsed)
			if us.MemLimit > 0 {
				row.Mem += " af " + bytesText(us.MemLimit)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MemBytes != rows[j].MemBytes {
			return rows[i].MemBytes > rows[j].MemBytes
		}
		return rows[i].Name < rows[j].Name
	})

	type part struct{ Name, Size, Hint string }
	parts := []part{
		{"Appenes data (/data)", humanSize(disk.Data), "Det, appene selv gemmer"},
		{"Backups", humanSize(disk.Backups), "De 10 nyeste pr. app"},
		{"Versioner (kildekode)", humanSize(disk.Sources), "De 5 nyeste pr. app"},
		{"Logfiler", humanSize(disk.Logs), "Roteres ved 2 MB"},
		{"Database (state.db)", humanSize(disk.State), "Konti, apps og indstillinger"},
	}
	for _, d := range df {
		parts = append(parts, part{"Docker: " + d.Type, d.Size, "Kan frigøres: " + d.Reclaimable})
	}

	since := ""
	if len(samples) > 0 {
		since = relTime(samples[0].T)
	}
	s.page(w, r, "server.html", "Server", map[string]any{
		"Tiles": tiles(h),
		"Charts": []chart{
			{"CPU", lineChart("CPU", samples, func(x Sample) float64 { return x.CPU })},
			{"Hukommelse", lineChart("Hukommelse", samples, func(x Sample) float64 { return x.Mem })},
			{"Disk", lineChart("Disk", samples, func(x Sample) float64 { return x.Disk })},
		},
		"Since":       since,
		"Uptime":      uptimeText(h.Uptime),
		"Measured":    !h.At.IsZero(),
		"Rows":        rows,
		"Parts":       parts,
		"Docker":      s.rt.Name() == "docker",
		"RefreshSecs": 30,
		"Self":        s.upd.View(),
	})
}

// serverUpdate er knapperne under «Vibe Deploy» på siden Server: opdater nu, tjek nu, automatisk til/fra.
func (s *Server) serverUpdate(w http.ResponseWriter, r *http.Request, u *User) {
	_ = r.ParseForm()
	msg := ""
	switch r.PostForm.Get("action") {
	case "now":
		if err := s.upd.Request("startet af " + u.Name); err != nil {
			s.page(w, r, "message.html", "Opdatering", map[string]any{"Text": err.Error(), "Back": "/server"})
			return
		}
		msg = "updating"
	case "check":
		s.upd.Check()
		msg = "checked"
	case "auto":
		on := r.PostForm.Get("auto") == "on"
		tz, err := tzFromForm(r.PostForm.Get("tz"), r.PostForm.Get("tz_other"))
		if err == nil {
			err = s.upd.SetSchedule(on, r.PostForm.Get("day"), r.PostForm.Get("at"), tz)
		}
		if err != nil {
			s.page(w, r, "message.html", "Opdatering", map[string]any{"Text": err.Error(), "Back": "/server"})
			return
		}
		msg = map[bool]string{true: "autoon", false: "autooff"}[on]
	}
	http.Redirect(w, r, "/server?m="+msg+"#vibedeploy", http.StatusSeeOther)
}

// chart er en graf med sin titel.
type chart struct {
	Name string
	SVG  template.HTML
}

// serverAlert er en advarsel til forsiden, når disken eller hukommelsen er ved at være fuld; ellers tom.
func (s *Server) serverAlert() string {
	h, _, _, _, _ := s.mon.Snapshot()
	var out []string
	if h.DiskOK && h.DiskTotal > 0 {
		if p := 100 * float64(h.DiskUsed) / float64(h.DiskTotal); p >= 90 {
			out = append(out, fmt.Sprintf("Disken er %s fuld (%s fri).", pctText(p), bytesText(h.DiskTotal-h.DiskUsed)))
		}
	}
	if h.MemOK && h.MemTotal > 0 {
		if p := 100 * float64(h.MemUsed) / float64(h.MemTotal); p >= 90 {
			out = append(out, fmt.Sprintf("Hukommelsen er %s brugt.", pctText(p)))
		}
	}
	return strings.Join(out, " ")
}

// appUsage er forbruget for én app til fanen Oversigt, eller nil, hvis der ikke er målt endnu.
func (s *Server) appUsage(slug string) map[string]string {
	us, ok := s.mon.App(slug)
	if !ok {
		return nil
	}
	out := map[string]string{"Data": humanSize(us.Data), "Backups": humanSize(us.Backups)}
	if us.Running {
		out["CPU"] = pctText(us.CPU)
		out["Mem"] = bytesText(us.MemUsed)
		if us.MemLimit > 0 {
			out["Mem"] += " af " + bytesText(us.MemLimit)
		}
	}
	return out
}

// ---------- graf ----------

const (
	chartW, chartH         = 320, 140
	padL, padR, padT, padB = 40, 6, 8, 24
	maxPoints              = 144 // højst ét punkt pr. 10 minutter over et døgn
	plotW, plotH           = chartW - padL - padR, chartH - padT - padB
	chartEmpty             = `<p class="muted small chart-empty">Samler målinger … Grafen kommer efter et minut.</p>`
)

// lineChart tegner én måling over tid (0–100 %) som SVG: en tynd linje med en svag flade under,
// en diskret skala og et punkt pr. tidsrum, der viser tid og værdi, når man holder musen over (title).
// Der er kun én serie pr. graf, så titlen over grafen navngiver den, og der er ingen forklaring.
func lineChart(name string, samples []Sample, pick func(Sample) float64) template.HTML {
	pts := downsample(samples, pick, maxPoints)
	if len(pts) < 2 {
		return template.HTML(chartEmpty)
	}
	t0, t1 := pts[0].T, pts[len(pts)-1].T
	span := t1.Sub(t0).Seconds()
	if span <= 0 {
		span = 1
	}
	x := func(t time.Time) float64 { return padL + plotW*t.Sub(t0).Seconds()/span }
	y := func(v float64) float64 { return padT + plotH*(1-clampPct(v)/100) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s det seneste %s">`, chartW, chartH, name, spanText(t1.Sub(t0)))
	for _, v := range []float64{0, 50, 100} {
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/><text class="axis" x="%d" y="%.1f" text-anchor="end">%.0f %%</text>`,
			padL, chartW-padR, y(v), y(v), padL-8, y(v)+4, v)
	}
	var line strings.Builder
	for i, p := range pts {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(p.T), y(p.V))
	}
	fmt.Fprintf(&b, `<polygon class="area" points="%.1f,%.1f %s %.1f,%.1f"/>`, x(t0), y(0), line.String(), x(t1), y(0))
	fmt.Fprintf(&b, `<polyline class="line" points="%s"/>`, line.String())
	last := pts[len(pts)-1]
	fmt.Fprintf(&b, `<circle class="dot last" cx="%.1f" cy="%.1f" r="4"/>`, x(last.T), y(last.V))
	fmt.Fprintf(&b, `<text class="axis" x="%d" y="%d">%s</text><text class="axis" x="%d" y="%d" text-anchor="end">nu</text>`,
		padL, chartH-6, t0.Format("15:04"), chartW-padR, chartH-6)

	// Hover: et usynligt felt pr. punkt, bredere end selve punktet, med en lodret hjælpelinje og en title.
	step := plotW / float64(len(pts)-1)
	for _, p := range pts {
		cx := x(p.T)
		fmt.Fprintf(&b, `<g class="hit"><rect x="%.1f" y="%d" width="%.1f" height="%d"/><line class="guide" x1="%.1f" x2="%.1f" y1="%d" y2="%d"/><circle class="dot" cx="%.1f" cy="%.1f" r="4"/><title>%s · %s</title></g>`,
			cx-step/2, padT, step, plotH, cx, cx, padT, padT+plotH, cx, y(p.V), p.T.Format("15:04"), pctText(p.V))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// point er ét punkt i en graf.
type point struct {
	T time.Time
	V float64
}

// downsample samler målingerne i højst n lige store tidsrum og tager gennemsnittet i hvert.
func downsample(s []Sample, pick func(Sample) float64, n int) []point {
	if len(s) <= n {
		out := make([]point, len(s))
		for i, x := range s {
			out[i] = point{x.T, pick(x)}
		}
		return out
	}
	var out []point
	per := float64(len(s)) / float64(n)
	for i := 0; i < n; i++ {
		a, b := int(float64(i)*per), int(float64(i+1)*per)
		if b > len(s) {
			b = len(s)
		}
		sum := 0.0
		for _, x := range s[a:b] {
			sum += pick(x)
		}
		out = append(out, point{s[b-1].T, sum / float64(b-a)})
	}
	return out
}

// spanText beskriver et tidsrum til skærmlæsere, fx «2 timer».
func spanText(d time.Duration) string {
	if d < time.Hour {
		m := int(d.Minutes())
		return fmt.Sprintf("%d %s", m, plural(m, "minut", "minutter"))
	}
	h := int(d.Hours())
	return fmt.Sprintf("%d %s", h, plural(h, "time", "timer"))
}
