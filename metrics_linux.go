package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// numCPU er antallet af kerner.
func numCPU() int { return runtime.NumCPU() }

// readCPU læser CPU-tællerne fra /proc/stat.
func readCPU() (cpuTicks, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTicks{}, false
	}
	return parseProcStat(string(b))
}

// readMem læser hukommelsen fra /proc/meminfo. «Brugt» er alt, der ikke er til rådighed (MemAvailable),
// så filcache, som styresystemet kan frigive, ikke tæller med.
func readMem() (used, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	return parseMeminfo(string(b))
}

// diskUsage er brugt og samlet plads på det filsystem, path ligger på (som df viser det).
func diskUsage(path string) (used, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	return total - st.Bfree*bs, total, total > 0
}

// readLoad læser belastningen (1, 5 og 15 min.) og oppetiden.
func readLoad() (load [3]float64, up time.Duration, ok bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return load, 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return load, 0, false
	}
	for i := 0; i < 3; i++ {
		load[i], _ = strconv.ParseFloat(f[i], 64)
	}
	if u, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(u)); len(f) > 0 {
			s, _ := strconv.ParseFloat(f[0], 64)
			up = time.Duration(s) * time.Second
		}
	}
	return load, up, true
}
