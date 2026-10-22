package main

import (
	"bufio"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Monitor måler serverens CPU, hukommelse og disk hvert 30. sekund og husker det seneste døgn
// i hukommelsen (historikken starter forfra, når serveren genstartes). Den måler også hver apps
// forbrug: CPU og hukommelse fra Docker og pladsen, appens filer fylder på disken.
type Monitor struct {
	dataDir string
	rt      Runtime

	mu       sync.Mutex
	samples  []Sample
	now      HostStats
	prevCPU  cpuTicks
	apps     map[string]AppUsage // slug → forbrug
	disk     DiskBreakdown
	diskAt   time.Time
	dockerDF []DockerDF
}

// Sample er én måling i historikken (procent af det hele).
type Sample struct {
	T              time.Time
	CPU, Mem, Disk float64
}

// HostStats er serverens seneste måling. OK-felterne er falske, hvor platformen ikke kan måles.
type HostStats struct {
	At                  time.Time
	CPU                 float64 // procent af alle kerner
	Cores               int
	CPUOK               bool
	MemUsed, MemTotal   uint64
	MemOK               bool
	DiskUsed, DiskTotal uint64
	DiskOK              bool
	Load                [3]float64
	LoadOK              bool
	Uptime              time.Duration
}

// AppUsage er én apps forbrug. CPU og hukommelse kommer fra Docker (kun når den kører).
type AppUsage struct {
	CPU                    float64
	MemUsed, MemLimit      uint64
	Running                bool
	Data, Backups, Sources int64 // bytes på disken
}

// DiskBreakdown er, hvad Vibe Deploys egne filer fylder.
type DiskBreakdown struct {
	Data, Backups, Sources, Logs, State int64
}

// DockerDF er én linje fra «docker system df» (images, containere, build-cache).
type DockerDF struct{ Type, Size, Reclaimable string }

// cpuTicks er tællere fra styresystemet; forbrug = 1 - Δidle/Δtotal.
type cpuTicks struct{ idle, total uint64 }

const (
	sampleEvery = 30 * time.Second
	keepHistory = 24 * time.Hour
	diskEvery   = 10 * time.Minute
)

// NewMonitor laver en Monitor; Run starter målingerne.
func NewMonitor(dataDir string, rt Runtime) *Monitor {
	return &Monitor{dataDir: dataDir, rt: rt, apps: map[string]AppUsage{}}
}

// Run måler med det samme og derefter hvert 30. sekund. Kører i sin egen goroutine.
func (m *Monitor) Run() {
	m.sample()
	for range time.Tick(sampleEvery) {
		m.sample()
	}
}

// sample tager én måling af serveren og appene og lægger den i historikken.
func (m *Monitor) sample() {
	h := HostStats{At: time.Now(), Cores: numCPU()}
	ticks, ok := readCPU()
	m.mu.Lock()
	prev := m.prevCPU
	m.prevCPU = ticks
	m.mu.Unlock()
	if ok && prev.total > 0 && ticks.total > prev.total {
		h.CPU = 100 * (1 - float64(ticks.idle-prev.idle)/float64(ticks.total-prev.total))
		h.CPU = clampPct(h.CPU)
		h.CPUOK = true
	}
	h.MemUsed, h.MemTotal, h.MemOK = readMem()
	h.DiskUsed, h.DiskTotal, h.DiskOK = diskUsage(m.dataDir)
	h.Load, h.Uptime, h.LoadOK = readLoad()

	containers := map[string]AppUsage{}
	if d, ok := m.rt.(*dockerRT); ok {
		containers = d.Stats()
	}

	m.mu.Lock()
	needDisk := time.Since(m.diskAt) > diskEvery
	m.mu.Unlock()
	var disk DiskBreakdown
	var perApp map[string]AppUsage
	var df []DockerDF
	if needDisk {
		disk, perApp = m.measureDisk()
		if d, ok := m.rt.(*dockerRT); ok {
			df = d.SystemDF()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = h
	if needDisk {
		m.disk, m.diskAt, m.dockerDF = disk, time.Now(), df
		for slug, u := range perApp {
			c := m.apps[slug]
			c.Data, c.Backups, c.Sources = u.Data, u.Backups, u.Sources
			m.apps[slug] = c
		}
	}
	for slug, c := range m.apps {
		c.CPU, c.MemUsed, c.MemLimit, c.Running = 0, 0, 0, false
		m.apps[slug] = c
	}
	for slug, u := range containers {
		c := m.apps[slug]
		c.CPU, c.MemUsed, c.MemLimit, c.Running = u.CPU, u.MemUsed, u.MemLimit, true
		m.apps[slug] = c
	}
	s := Sample{T: h.At}
	if h.CPUOK {
		s.CPU = h.CPU
	}
	if h.MemOK && h.MemTotal > 0 {
		s.Mem = 100 * float64(h.MemUsed) / float64(h.MemTotal)
	}
	if h.DiskOK && h.DiskTotal > 0 {
		s.Disk = 100 * float64(h.DiskUsed) / float64(h.DiskTotal)
	}
	if h.CPUOK || h.MemOK || h.DiskOK {
		m.samples = append(m.samples, s)
	}
	cut := 0
	for cut < len(m.samples) && time.Since(m.samples[cut].T) > keepHistory {
		cut++
	}
	m.samples = m.samples[cut:]
}

// measureDisk lægger størrelsen af data-mappens undermapper sammen, i alt og pr. app.
func (m *Monitor) measureDisk() (DiskBreakdown, map[string]AppUsage) {
	per := map[string]AppUsage{}
	var d DiskBreakdown
	add := func(sub string, set func(u *AppUsage, n int64), total *int64) {
		entries, _ := filepath.Glob(filepath.Join(m.dataDir, sub, "*"))
		for _, e := range entries {
			n := dirSize(e)
			*total += n
			slug := filepath.Base(e)
			if set != nil && slugRe.MatchString(slug) {
				u := per[slug]
				set(&u, n)
				per[slug] = u
			}
		}
	}
	add("volumes", func(u *AppUsage, n int64) { u.Data = n }, &d.Data)
	add("backups", func(u *AppUsage, n int64) { u.Backups = n }, &d.Backups)
	add("apps", func(u *AppUsage, n int64) { u.Sources = n }, &d.Sources)
	add("logs", nil, &d.Logs)
	for _, f := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		d.State += dirSize(filepath.Join(m.dataDir, f))
	}
	return d, per
}

// dirSize er den samlede størrelse af almindelige filer under path (eller filen selv).
func dirSize(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// Snapshot returnerer en kopi af de seneste målinger, til visning.
func (m *Monitor) Snapshot() (HostStats, []Sample, map[string]AppUsage, DiskBreakdown, []DockerDF) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apps := make(map[string]AppUsage, len(m.apps))
	for k, v := range m.apps {
		apps[k] = v
	}
	return m.now, append([]Sample(nil), m.samples...), apps, m.disk, append([]DockerDF(nil), m.dockerDF...)
}

// App returnerer én apps forbrug.
func (m *Monitor) App(slug string) (AppUsage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.apps[slug]
	return u, ok
}

// Stats henter CPU og hukommelse for alle Vibe Deploy-containere (vd-<slug>) med «docker stats».
func (d *dockerRT) Stats() map[string]AppUsage {
	out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}").Output()
	if err != nil {
		return nil
	}
	return parseDockerStats(string(out))
}

// SystemDF henter, hvad Docker selv fylder (images, containere, build-cache).
func (d *dockerRT) SystemDF() []DockerDF {
	out, err := exec.Command("docker", "system", "df", "--format", "{{.Type}}\t{{.Size}}\t{{.Reclaimable}}").Output()
	if err != nil {
		return nil
	}
	var df []DockerDF
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			df = append(df, DockerDF{f[0], f[1], f[2]})
		}
	}
	return df
}

// parseDockerStats læser linjer som «vd-blog\t0.15%\t12.5MiB / 512MiB». Andre containere springes over.
// Appens to pladser (vd-blog og vd-blog_b) tælles som én app.
func parseDockerStats(out string) map[string]AppUsage {
	res := map[string]AppUsage{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		name := strings.TrimSpace(f[0])
		if !strings.HasPrefix(name, "vd-") {
			continue
		}
		slug := appOf(name) // vd-blog og vd-blog_b er samme app
		u := AppUsage{Running: true}
		u.CPU, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(f[1]), "%"), 64)
		if used, limit, ok := strings.Cut(f[2], "/"); ok {
			u.MemUsed, u.MemLimit = parseSize(used), parseSize(limit)
		}
		if prev, ok := res[slug]; ok { // kort overlap under en udrulning: læg de to containere sammen
			u.CPU += prev.CPU
			u.MemUsed += prev.MemUsed
		}
		res[slug] = u
	}
	return res
}

// parseSize læser en størrelse fra Docker, fx «12.5MiB», «1.2GB» eller «512B».
func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suf  string
		mult float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suf); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil {
				return 0
			}
			return uint64(v * u.mult)
		}
	}
	return 0
}

// parseProcStat læser den samlede cpu-linje fra /proc/stat. idle tæller idle + iowait.
func parseProcStat(s string) (cpuTicks, bool) {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTicks
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			if i >= 8 { // guest-tid er allerede talt med i user/nice
				break
			}
			t.total += n
			if i == 3 || i == 4 {
				t.idle += n
			}
		}
		return t, true
	}
	return cpuTicks{}, false
}

// parseMeminfo læser MemTotal og MemAvailable (i kB) fra /proc/meminfo og returnerer bytes.
func parseMeminfo(s string) (used, total uint64, ok bool) {
	var avail uint64
	var haveAvail bool
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			avail, haveAvail = n*1024, true
		}
	}
	if total == 0 || !haveAvail || avail > total {
		return 0, 0, false
	}
	return total - avail, total, true
}

// clampPct holder en procent mellem 0 og 100.
func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
