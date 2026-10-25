package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// En database i WAL-tilstand, som appen har åben og skriver i, skal komme med som én hel fil i backuppen.
func TestBackupSQLiteInUse(t *testing.T) {
	m := newTestManager(t)
	vol, err := m.ensureVolume("app")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(vol, "app.db")+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := db.Exec(`INSERT INTO t VALUES (?)`, i); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(vol, "noter.txt"), []byte("hej"), 0o644)
	if !fileExists(filepath.Join(vol, "app.db-wal")) {
		t.Fatal("testen forventer, at rækkerne ligger i -wal")
	}
	time.Sleep(150 * time.Millisecond) // databasefilen må ikke være lige ændret (se snapshotSQLite)

	name, err := m.Backup("app", "test")
	if err != nil || name == "" {
		t.Fatalf("backup: %q %v", name, err)
	}
	f, err := os.Open(filepath.Join(m.backupDir("app"), name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := t.TempDir()
	if _, _, err := extractTarGz(f, out, 1<<30, 100); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"app.db-wal", "app.db-shm"} {
		if fileExists(filepath.Join(out, side)) {
			t.Errorf("%s burde ikke være med i backuppen", side)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(out, "noter.txt")); string(b) != "hej" {
		t.Errorf("noter.txt: %q", b)
	}
	cp, err := sql.Open("sqlite", "file:"+filepath.Join(out, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var n int
	if err := cp.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 100 {
		t.Fatalf("rækker i kopien: %d %v", n, err)
	}
	if left, _ := filepath.Glob(filepath.Join(m.backupDir("app"), ".sqlite-*")); len(left) > 0 {
		t.Errorf("midlertidige filer er ikke ryddet op: %v", left)
	}
}

// Den daglige backup regnes fra den nyeste kopi af state.db, så genstarter ikke nulstiller den.
func TestDailyDue(t *testing.T) {
	m := newTestManager(t)
	now := time.Now()
	if !m.dailyDue(now, time.Time{}) {
		t.Error("uden tidligere backup skal den køre")
	}
	if m.dailyDue(now, now.Add(-time.Hour)) {
		t.Error("et forsøg for en time siden i denne proces skal vente")
	}
	dir := filepath.Join(m.dataDir, "backups", "_server")
	os.MkdirAll(dir, 0o700)
	write := func(at time.Time) {
		os.WriteFile(filepath.Join(dir, "state-"+at.Format("20060102-150405")+".db"), nil, 0o600)
	}
	write(now.Add(-30 * time.Hour))
	if !m.dailyDue(now, time.Time{}) {
		t.Error("30 timer siden: skal køre")
	}
	write(now.Add(-2 * time.Hour))
	if m.dailyDue(now, time.Time{}) {
		t.Error("2 timer siden: skal vente")
	}
	if !m.dailyDue(now.Add(23*time.Hour), time.Time{}) {
		t.Error("25 timer efter: skal køre")
	}
}

// En backup kan hentes som fil af den, der styrer appen, men ikke af andre, og navnet kan ikke pege uden for mappen.
func TestBackupDownload(t *testing.T) {
	s, owner, _ := newResetServer(t)
	if _, err := s.mgr.Deploy("Blog", 8080, map[string]string{"index.html": "hej"}, "", owner.ID); err != nil {
		t.Fatal(err)
	}
	vol, _ := s.mgr.ensureVolume("blog")
	os.WriteFile(filepath.Join(vol, "noter.txt"), []byte("hej"), 0o644)
	name, err := s.mgr.Backup("blog", "test")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "min-server.dk"
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	w := get("/apps/blog/backups/"+name, "s-o")
	if w.Code != http.StatusOK || w.Header().Get("Content-Disposition") != `attachment; filename="blog-`+name+`"` {
		t.Fatalf("ejer: %d %q", w.Code, w.Header().Get("Content-Disposition"))
	}
	if _, _, err := extractTarGz(w.Body, t.TempDir(), 1<<20, 10); err != nil {
		t.Fatalf("den hentede fil kan ikke pakkes ud: %v", err)
	}
	if w := get("/apps/blog/backups/"+name, "s-m"); w.Code == http.StatusOK || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("medlem uden adgang fik backuppen: %d", w.Code)
	}
	for _, bad := range []string{"..%2F..%2Fstate.db", "20990101-000000.tar.gz", "noget.tar.gz"} {
		if w := get("/apps/blog/backups/"+bad, "s-o"); w.Code == http.StatusOK {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
}
