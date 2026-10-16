package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkTarGz(t *testing.T, entries []entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractOK(t *testing.T) {
	dir := t.TempDir()
	n, skipped, err := extractTarGz(mkTarGz(t, []entry{
		{name: "./", typ: tar.TypeDir},
		{name: "./Dockerfile", body: "FROM x"},
		{name: "./src/main.go", body: "package main"},
		{name: "./start.sh", body: "#!/bin/sh", mode: 0o755},
		{name: "./link", typ: tar.TypeSymlink, link: "/etc/passwd"},
		{name: "./._Dockerfile", body: "mac-rest"},
		{name: "./.DS_Store", body: "x"},
	}), dir, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("forventede 3 filer, fik %d", n)
	}
	if len(skipped) != 1 || skipped[0] != "link" {
		t.Errorf("symlink skulle springes over: %v", skipped)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("symlink må ikke oprettes")
	}
	if _, err := os.Stat(filepath.Join(dir, "._Dockerfile")); err == nil {
		t.Error("macOS-rester skal springes over")
	}
	if st, _ := os.Stat(filepath.Join(dir, "start.sh")); st == nil || st.Mode()&0o111 == 0 {
		t.Error("kørselsret skal bevares")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "src/main.go")); string(b) != "package main" {
		t.Error("forkert indhold")
	}
}

func TestExtractRejects(t *testing.T) {
	cases := map[string][]entry{
		"sti ud af mappen":  {{name: "../evil", body: "x"}},
		"dybt ud af mappen": {{name: "a/../../evil", body: "x"}},
		"absolut sti":       {{name: "/etc/evil", body: "x"}},
		"for stor":          {{name: "big", body: strings.Repeat("x", 2000)}},
		"for mange filer":   {{name: "a", body: "1"}, {name: "b", body: "2"}, {name: "c", body: "3"}},
		"tom pakke":         {},
	}
	for name, entries := range cases {
		dir := t.TempDir()
		_, _, err := extractTarGz(mkTarGz(t, entries), dir, 1000, 2)
		if err == nil {
			t.Errorf("%s: burde afvises", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Errorf("%s: fil skrevet uden for mappen!", name)
		}
	}
	if _, _, err := extractTarGz(strings.NewReader("ikke gzip"), t.TempDir(), 1000, 10); err == nil {
		t.Error("ugyldig gzip burde afvises")
	}
}
