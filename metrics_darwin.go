package main

// Målinger på macOS, så dashboardet også virker, når man udvikler lokalt. Bruger sysctl, vm_stat og top,
// da macOS ikke har /proc.

import (
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// numCPU er antallet af kerner.
func numCPU() int { return runtime.NumCPU() }

var (
	darwinCPUMu sync.Mutex
	darwinCPU   cpuTicks // opsummerede tællere bygget af tops procenter
	topIdleRe   = regexp.MustCompile(`([\d.]+)% idle`)
)

// readCPU spørger top om CPU-forbruget lige nu og lægger det til nogle opdigtede tællere, så
// beregningen i Monitor er den samme som på Linux.
func readCPU() (cpuTicks, bool) {
	out, err := exec.Command("top", "-l", "2", "-n", "0", "-s", "1").Output()
	if err != nil {
		return cpuTicks{}, false
	}
	m := topIdleRe.FindAllStringSubmatch(string(out), -1)
	if len(m) == 0 {
		return cpuTicks{}, false
	}
	idle, _ := strconv.ParseFloat(m[len(m)-1][1], 64) // første måling fra top er fra opstart; brug den sidste
	darwinCPUMu.Lock()
	defer darwinCPUMu.Unlock()
	darwinCPU.total += 10000
	darwinCPU.idle += uint64(idle * 100)
	return darwinCPU, true
}

// readMem bruger hw.memsize og vm_stat. «Brugt» er aktive, faste og komprimerede sider.
func readMem() (used, total uint64, ok bool) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, false
	}
	total, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	vs, err := exec.Command("vm_stat").Output()
	if err != nil || total == 0 {
		return 0, 0, false
	}
	page := uint64(4096)
	if m := regexp.MustCompile(`page size of (\d+) bytes`).FindStringSubmatch(string(vs)); m != nil {
		page, _ = strconv.ParseUint(m[1], 10, 64)
	}
	pages := func(name string) uint64 {
		m := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s+(\d+)`).FindStringSubmatch(string(vs))
		if m == nil {
			return 0
		}
		n, _ := strconv.ParseUint(m[1], 10, 64)
		return n
	}
	used = (pages("Pages active") + pages("Pages wired down") + pages("Pages occupied by compressor")) * page
	if used > total {
		used = total
	}
	return used, total, true
}

// diskUsage er brugt og samlet plads på det filsystem, path ligger på.
func diskUsage(path string) (used, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	return total - st.Bfree*bs, total, total > 0
}

// readLoad læser belastningen fra vm.loadavg og oppetiden fra kern.boottime.
func readLoad() (load [3]float64, up time.Duration, ok bool) {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return load, 0, false
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) < 3 {
		return load, 0, false
	}
	for i := 0; i < 3; i++ {
		load[i], _ = strconv.ParseFloat(f[i], 64)
	}
	if b, err := exec.Command("sysctl", "-n", "kern.boottime").Output(); err == nil {
		if m := regexp.MustCompile(`sec = (\d+)`).FindStringSubmatch(string(b)); m != nil {
			sec, _ := strconv.ParseInt(m[1], 10, 64)
			up = time.Since(time.Unix(sec, 0)).Truncate(time.Second)
		}
	}
	return load, up, true
}
