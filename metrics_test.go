package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	a, ok := parseProcStat("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n")
	if !ok || a.total != 1000 || a.idle != 800 {
		t.Fatalf("fik %+v", a)
	}
	b, _ := parseProcStat("cpu  200 0 200 1300 100 0 0 0 0 0\n")
	if got := 100 * (1 - float64(b.idle-a.idle)/float64(b.total-a.total)); got < 24.9 || got > 25.1 {
		t.Fatalf("CPU burde være 25 %%, fik %.1f", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	used, total, ok := parseMeminfo("MemTotal:  4000000 kB\nMemFree:  100000 kB\nMemAvailable: 1000000 kB\n")
	if !ok || total != 4000000*1024 || used != 3000000*1024 {
		t.Fatalf("fik %d/%d", used, total)
	}
	if _, _, ok := parseMeminfo("MemTotal: 10 kB\n"); ok {
		t.Fatal("uden MemAvailable kan forbruget ikke beregnes")
	}
}

func TestParseDockerStats(t *testing.T) {
	got := parseDockerStats("vd-blog\t0.15%\t12.5MiB / 512MiB\nandet\t5%\t1GiB / 2GiB\n")
	if len(got) != 1 {
		t.Fatalf("kun vd-containere skal med: %+v", got)
	}
	b := got["blog"]
	if b.CPU != 0.15 || b.MemUsed != uint64(12.5*(1<<20)) || b.MemLimit != 512<<20 || !b.Running {
		t.Fatalf("fik %+v", b)
	}
	if parseSize("1.5GB") != 1_500_000_000 || parseSize("0B") != 0 || parseSize("??") != 0 {
		t.Fatal("parseSize")
	}
}

func TestDownsampleAndChart(t *testing.T) {
	now := time.Now()
	var s []Sample
	for i := 0; i < 1000; i++ {
		s = append(s, Sample{T: now.Add(time.Duration(i-1000) * 30 * time.Second), CPU: float64(i % 100)})
	}
	pts := downsample(s, func(x Sample) float64 { return x.CPU }, maxPoints)
	if len(pts) != maxPoints {
		t.Fatalf("fik %d punkter", len(pts))
	}
	svg := string(lineChart("CPU", s, func(x Sample) float64 { return x.CPU }))
	if !strings.HasPrefix(svg, "<svg") || strings.Count(svg, `class="hit"`) != maxPoints {
		t.Fatal("grafen skal have ét hover-felt pr. punkt")
	}
	if string(lineChart("CPU", s[:1], func(x Sample) float64 { return x.CPU })) != chartEmpty {
		t.Fatal("ét punkt er ikke nok til en graf")
	}
}

func TestMonitorSamples(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "volumes", "blog"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "volumes", "blog", "a.db"), make([]byte, 5000), 0o600)
	m := NewMonitor(dir, newDemoRT())
	m.sample()
	if u, ok := m.App("blog"); !ok || u.Data != 5000 {
		t.Fatalf("appens data skal måles: %+v", u)
	}
	_, _, _, disk, _ := m.Snapshot()
	if disk.Data != 5000 {
		t.Fatalf("fik %+v", disk)
	}
}

func TestDockerfileWarnings(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) { _ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(s), 0o644) }
	write("FROM node:22\nEXPOSE 3000\nCMD [\"node\",\"server.js\"]\n")
	if w := dockerfileWarnings(dir, 8080); len(w) != 1 || !strings.Contains(w[0], "3000") {
		t.Fatalf("forkert port skal give en advarsel: %v", w)
	}
	if w := dockerfileWarnings(dir, 3000); len(w) != 0 {
		t.Fatalf("rigtig port: %v", w)
	}
	write("FROM python:3.12\nCMD gunicorn -b 127.0.0.1:8080 app:app\n")
	if w := dockerfileWarnings(dir, 8080); len(w) != 1 || !strings.Contains(w[0], "0.0.0.0") {
		t.Fatalf("localhost skal give en advarsel: %v", w)
	}
	if !strings.Contains(buildGuide("node"), "npm ci") || !strings.Contains(buildGuide(""), "Skabeloner findes til: go, node, python, static") {
		t.Fatal("byggevejledningen")
	}
}
