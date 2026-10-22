package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.3.0", "v0.2.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v0.3.0", "dev", false}, // en udviklingsudgave opdaterer sig ikke
		{"dev", "v0.1.0", false},
		{"v0.3.0-rc1", "v0.2.0", false},
		{"v01.2.3", "v0.1.0", false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, ventede %v", c.a, c.b, got, c.want)
		}
	}
}

// release er en falsk release-server med signeret manifest og binær.
type release struct {
	srv          *httptest.Server
	pub          ed25519.PublicKey
	ver, binBody string
}

func newRelease(t *testing.T, ver, binBody string, tamper func(raw []byte) []byte) *release {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, binaryName()), []byte(binBody), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := buildManifest(dir, ver, "https://example.com/notes", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(signContext), raw...)))
	if tamper != nil {
		raw = tamper(raw)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/release.json", func(w http.ResponseWriter, r *http.Request) { w.Write(raw) })
	mux.HandleFunc("/release.json.sig", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, sig) })
	mux.HandleFunc("/"+binaryName(), func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, binBody) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &release{srv: srv, pub: pub, ver: ver, binBody: binBody}
}

func TestReleaseSignature(t *testing.T) {
	r := newRelease(t, "v0.3.0", "NY", nil)
	m, err := fetchRelease(r.srv.Client(), r.srv.URL, r.pub)
	if err != nil || m.Version != "v0.3.0" || m.Notes == "" {
		t.Fatalf("en ægte release burde godkendes: %+v %v", m, err)
	}
	sum := sha256.Sum256([]byte("NY"))
	if m.Files[binaryName()] != hex.EncodeToString(sum[:]) {
		t.Fatal("manifestet skal have binærens SHA-256")
	}

	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := fetchRelease(r.srv.Client(), r.srv.URL, otherPub); err == nil {
		t.Fatal("en release signeret med en anden nøgle skal afvises")
	}
	if _, err := fetchRelease(r.srv.Client(), r.srv.URL, nil); err == nil || !strings.Contains(err.Error(), "releasekey.go") {
		t.Fatalf("uden indbygget nøgle skal der komme en forklaring, fik %v", err)
	}
	// Én ændret byte i manifestet (fx en ældre version eller en anden SHA-256) skal afvises.
	bad := newRelease(t, "v0.3.0", "NY", func(raw []byte) []byte { return bytes.Replace(raw, []byte("v0.3.0"), []byte("v0.9.0"), 1) })
	if _, err := fetchRelease(bad.srv.Client(), bad.srv.URL, bad.pub); err == nil {
		t.Fatal("et ændret manifest skal afvises")
	}
}

func TestDownloadVerified(t *testing.T) {
	r := newRelease(t, "v0.3.0", "NY", nil)
	m, _ := fetchRelease(r.srv.Client(), r.srv.URL, r.pub)
	dst := filepath.Join(t.TempDir(), "vibedeploy")
	if err := downloadVerified(r.srv.Client(), r.srv.URL, m, binaryName(), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "NY" {
		t.Fatalf("forkert indhold: %q", b)
	}
	m.Files[binaryName()] = strings.Repeat("0", 64)
	dst2 := filepath.Join(filepath.Dir(dst), "anden")
	if err := downloadVerified(r.srv.Client(), r.srv.URL, m, binaryName(), dst2); err == nil || fileExists(dst2) {
		t.Fatal("en fil med forkert SHA-256 må ikke lægges på plads")
	}
	if fileBase("https://github.com/a/b/releases/latest/download", "v1.2.3") != "https://github.com/a/b/releases/download/v1.2.3" {
		t.Fatal("binæren skal hentes fra netop den release, manifestet beskriver")
	}
}

// fakeHost er systemctl og tjenesten i testene af «vibedeploy update».
type fakeHost struct {
	mu      sync.Mutex
	calls   []string
	running bool
	// healthyWhen afgør, om tjenesten svarer med versionen want, ud fra den installerede binær.
	healthyWhen func(want, binBody string) bool
}

func (h *fakeHost) systemctl(args ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, strings.Join(args, " "))
	h.running = args[0] == "start"
	return nil
}

func newSelfUpdate(t *testing.T, r *release, current string, h *fakeHost) *selfUpdate {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "vibedeploy")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	_ = os.WriteFile(bin, []byte("GAMMEL"), 0o755)
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o700)
	_ = os.WriteFile(filepath.Join(data, "state.db"), []byte("NUVÆRENDE"), 0o600)
	_ = os.WriteFile(filepath.Join(data, updateRequestName), []byte("{}"), 0o644)
	u := &selfUpdate{dataDir: data, bin: bin, client: r.srv.Client(), base: r.srv.URL, pub: r.pub, current: current,
		systemctl: h.systemctl, log: io.Discard,
		probe: func(p string) (string, error) {
			b, _ := os.ReadFile(p)
			if string(b) == r.binBody {
				return r.ver, nil
			}
			return "", nil
		}}
	u.healthy = func(want string, d time.Duration) bool {
		b, _ := os.ReadFile(bin)
		return h.running && h.healthyWhen(want, string(b))
	}
	return u
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<mangler>"
	}
	return string(b)
}

func TestSelfUpdateOK(t *testing.T) {
	r := newRelease(t, "v0.3.0", "NY", nil)
	h := &fakeHost{healthyWhen: func(want, bin string) bool { return want == "v0.3.0" && bin == "NY" }}
	u := newSelfUpdate(t, r, "v0.2.0", h)
	if err := u.run(); err != nil {
		t.Fatal(err)
	}
	if read(t, u.bin) != "NY" || read(t, u.bin+".prev") != "GAMMEL" {
		t.Fatalf("den nye binær skal være på plads og den gamle gemt: %q / %q", read(t, u.bin), read(t, u.bin+".prev"))
	}
	if st := readUpdateStatus(u.dataDir); st.State != "ok" || st.From != "v0.2.0" || st.To != "v0.3.0" {
		t.Fatalf("status: %+v", st)
	}
	if fileExists(filepath.Join(u.dataDir, updateRequestName)) || fileExists(u.bin+".new") {
		t.Fatal("anmodningen og den midlertidige fil skal være væk")
	}
	if strings.Join(h.calls, ",") != "stop vibedeploy,start vibedeploy" {
		t.Fatalf("systemctl: %v", h.calls)
	}
}

func TestSelfUpdateNothingNew(t *testing.T) {
	r := newRelease(t, "v0.2.0", "SAMME", nil)
	h := &fakeHost{}
	u := newSelfUpdate(t, r, "v0.2.0", h)
	if err := u.run(); err != nil {
		t.Fatal(err)
	}
	if read(t, u.bin) != "GAMMEL" || len(h.calls) != 0 || readUpdateStatus(u.dataDir).State != "none" {
		t.Fatalf("intet må ændres, når der ikke er en ny version: %v", h.calls)
	}
}

func TestSelfUpdateRefusesBadBinary(t *testing.T) {
	r := newRelease(t, "v0.3.0", "NY", nil)
	h := &fakeHost{}
	u := newSelfUpdate(t, r, "v0.2.0", h)
	u.probe = func(string) (string, error) { return "v0.1.0", nil } // binæren påstår at være en anden version
	if err := u.run(); err == nil {
		t.Fatal("ventede en fejl")
	}
	if read(t, u.bin) != "GAMMEL" || len(h.calls) != 0 || readUpdateStatus(u.dataDir).State != "failed" {
		t.Fatal("tjenesten må ikke røres, når den nye binær ikke kan bekræfte sin version")
	}
}

// Den nye version svarer ikke, men den gamle starter fint på de nuværende data: data røres ikke.
func TestSelfUpdateRollbackKeepsData(t *testing.T) {
	r := newRelease(t, "v0.3.0", "NY", nil)
	h := &fakeHost{healthyWhen: func(want, bin string) bool { return want == "v0.2.0" && bin == "GAMMEL" }}
	u := newSelfUpdate(t, r, "v0.2.0", h)
	_ = os.WriteFile(filepath.Join(u.dataDir, updateBackupName), []byte("BACKUP"), 0o600)
	if err := u.run(); err == nil {
		t.Fatal("ventede en fejl")
	}
	if read(t, u.bin) != "GAMMEL" || read(t, u.bin+".failed") != "NY" {
		t.Fatalf("den gamle binær skal tilbage: %q / %q", read(t, u.bin), read(t, u.bin+".failed"))
	}
	if read(t, filepath.Join(u.dataDir, "state.db")) != "NUVÆRENDE" {
		t.Fatal("data må ikke røres, når den gamle version kan starte på dem")
	}
	if st := readUpdateStatus(u.dataDir); st.State != "rolledback" || !strings.Contains(st.Message, "Ingen data er rørt") {
		t.Fatalf("status: %+v", st)
	}
}

// Den nye version har ændret data, så den gamle ikke kan starte: backuppen af state.db lægges tilbage.
func TestSelfUpdateRollbackRestoresBackup(t *testing.T) {
	r := newRelease(t, "v0.3.0", "NY", nil)
	var u *selfUpdate
	h := &fakeHost{}
	h.healthyWhen = func(want, bin string) bool {
		return want == "v0.2.0" && bin == "GAMMEL" && read(t, filepath.Join(u.dataDir, "state.db")) == "BACKUP"
	}
	u = newSelfUpdate(t, r, "v0.2.0", h)
	db := filepath.Join(u.dataDir, "state.db")
	_ = os.WriteFile(filepath.Join(u.dataDir, updateBackupName), []byte("BACKUP"), 0o600)
	_ = os.WriteFile(db+"-wal", []byte("wal"), 0o600)
	if err := u.run(); err == nil {
		t.Fatal("ventede en fejl")
	}
	if read(t, u.bin) != "GAMMEL" || read(t, db) != "BACKUP" || read(t, db+".efter-opdatering") != "NUVÆRENDE" || fileExists(db+"-wal") {
		t.Fatalf("binær %q, state.db %q, efter %q", read(t, u.bin), read(t, db), read(t, db+".efter-opdatering"))
	}
	if st := readUpdateStatus(u.dataDir); st.State != "rolledback" || !strings.Contains(st.Message, "data fra lige før opdateringen") {
		t.Fatalf("status: %+v", st)
	}
}

// setVersion sætter den kørende version i en test og lægger den tilbage bagefter.
func setVersion(t *testing.T, v string) {
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

func newTestUpdater(t *testing.T, r *release) (*Updater, *Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(dir, updatePathUnit)
	_ = os.WriteFile(unit, nil, 0o644)
	old := updateUnitFile
	updateUnitFile = unit
	t.Cleanup(func() { updateUnitFile = old })
	return &Updater{dataDir: dir, st: st, client: r.srv.Client(), base: r.srv.URL, pub: r.pub, now: time.Now}, st
}

func TestUpdaterRequest(t *testing.T) {
	setVersion(t, "v0.2.0")
	r := newRelease(t, "v0.3.0", "NY", nil)
	u, _ := newTestUpdater(t, r)
	u.Check()
	if m := u.Latest(); m == nil || m.Version != "v0.3.0" {
		t.Fatalf("burde se v0.3.0: %+v", m)
	}
	if err := u.Request("test"); err != nil {
		t.Fatal(err)
	}
	if !u.Pending() || !fileExists(filepath.Join(u.dataDir, updateBackupName)) {
		t.Fatal("en anmodning skal tage backup af state.db og lægge update-request")
	}
	var req map[string]any
	_ = json.Unmarshal([]byte(read(t, filepath.Join(u.dataDir, updateRequestName))), &req)
	if req["to"] != "v0.3.0" || req["from"] != "v0.2.0" {
		t.Fatalf("anmodning: %v", req)
	}

	setVersion(t, "dev")
	_ = os.Remove(filepath.Join(u.dataDir, updateRequestName))
	if u.CanUpdate() || u.Request("test") == nil {
		t.Fatal("en udviklingsudgave må ikke opdatere sig selv")
	}
}

func TestUpdaterAutoOnSchedule(t *testing.T) {
	setVersion(t, "v0.2.0")
	r := newRelease(t, "v0.3.0", "NY", nil)
	u, st := newTestUpdater(t, r)
	cphTZ, _ := time.LoadLocation("Europe/Copenhagen")
	cph := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, cphTZ) } // okt. 2026: 25. er en søndag
	reset := func() {
		_ = os.Remove(filepath.Join(u.dataDir, updateRequestName))
		_ = st.Do(func(db *DB) error { db.Settings.LastAutoTry = time.Time{}; return nil })
	}

	u.now = func() time.Time { return cph(22, 3, 30) }
	u.tick()
	if u.Pending() {
		t.Fatal("automatisk opdatering er slået fra som standard")
	}
	if err := u.SetSchedule(true, "sun", "02:15", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"lørdag på klokkeslættet", cph(24, 2, 30), false},
		{"søndag før tidspunktet", cph(25, 2, 14), false},
		{"søndag på tidspunktet", cph(25, 2, 15), true},
		{"søndag inden for to timer", cph(25, 4, 14), true},
		{"søndag efter vinduet", cph(25, 4, 15), false},
	} {
		reset()
		u.now = func() time.Time { return c.at }
		u.tick()
		if u.Pending() != c.want {
			t.Errorf("%s: anmodning=%v, ventede %v", c.name, u.Pending(), c.want)
		}
	}

	// Ét forsøg pr. tidspunkt: fejler det (eller rulles tilbage), prøves det ikke igen hvert minut.
	reset()
	u.now = func() time.Time { return cph(25, 2, 20) }
	u.tick()
	_ = os.Remove(filepath.Join(u.dataDir, updateRequestName))
	u.now = func() time.Time { return cph(25, 2, 21) }
	u.tick()
	if u.Pending() {
		t.Fatal("samme tidspunkt må kun give ét forsøg")
	}
	u.now = func() time.Time { return cph(32, 2, 20) } // næste søndag (1. november)
	u.tick()
	if !u.Pending() {
		t.Fatal("næste søndag skal der prøves igen")
	}

	// En version, der er rullet tilbage, prøves ikke igen automatisk.
	reset()
	_ = os.WriteFile(filepath.Join(u.dataDir, updateStatusName), []byte(`{"State":"rolledback","To":"v0.3.0"}`), 0o644)
	u.now = func() time.Time { return cph(25, 2, 30) }
	u.tick()
	if u.Pending() {
		t.Fatal("en version, der er rullet tilbage, skal ikke prøves igen automatisk")
	}

	if err := u.SetSchedule(true, "sun", "25:00", ""); err == nil {
		t.Fatal("et ugyldigt klokkeslæt skal afvises")
	}
	if err := u.SetSchedule(true, "fredag", "03:00", ""); err == nil {
		t.Fatal("en ukendt dag skal afvises")
	}
}

func TestScheduleNextAndDanishTime(t *testing.T) {
	cphTZ, _ := time.LoadLocation("Europe/Copenhagen")
	cph := func(mo time.Month, d, h, m int) time.Time { return time.Date(2026, mo, d, h, m, 0, 0, cphTZ) }
	sc, _ := parseSchedule("sun", "03:00", "")
	if sc.tz != "Europe/Copenhagen" {
		t.Fatalf("standardtidszonen skal være Europe/Copenhagen, fik %q", sc.tz)
	}
	if got := danishWhen(sc.next(cph(10, 22, 12, 0), time.Time{}), sc.loc); got != "søndag 25. oktober kl. 03:00" {
		t.Fatalf("næste tidspunkt: %s", got)
	}
	// Klokkeslættet er dansk tid, også når serverens ur står på UTC: 03:00 i Danmark er 02:00 UTC om vinteren.
	if !sc.due(time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC), time.Time{}) || sc.due(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC).Add(-2*time.Hour), time.Time{}) {
		t.Fatal("tidspunktet skal regnes i dansk tid, ikke serverens")
	}
	// Natten mellem vinter- og sommertid (29. marts 2026 springer uret fra 02:00 til 03:00).
	daily, _ := parseSchedule("", "", "")
	if daily.describe() != "hver dag kl. 03:00" || !daily.due(cph(3, 29, 3, 10), time.Time{}) {
		t.Fatal("03:00 skal virke den nat, uret stilles")
	}
	if got := danishWhen(daily.next(cph(3, 29, 6, 0), time.Time{}), daily.loc); got != "mandag 30. marts kl. 03:00" {
		t.Fatalf("næste dag: %s", got)
	}
	if sc.describe() != "hver søndag kl. 03:00" {
		t.Fatalf("beskrivelse: %s", sc.describe())
	}
	var empty Settings
	if s := empty.schedule(); s.day != "daily" || s.hour != 3 || s.minute != 0 || s.tz != "Europe/Copenhagen" {
		t.Fatalf("standard skal være hver dag kl. 03:00 i Europe/Copenhagen: %+v", s)
	}
}

// Ejeren kan vælge en anden tidszone; tidspunktet følger dens ur og sommertid.
func TestScheduleOtherTimezone(t *testing.T) {
	ny, err := parseSchedule("daily", "03:00", " America/New_York ")
	if err != nil || ny.tz != "America/New_York" {
		t.Fatalf("America/New_York skal kunne vælges: %+v %v", ny, err)
	}
	// 03:00 i New York er 07:00 UTC om sommeren (EDT) og 08:00 UTC om vinteren (EST).
	if !ny.due(time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC), time.Time{}) || ny.due(time.Date(2026, 7, 1, 6, 59, 0, 0, time.UTC), time.Time{}) {
		t.Fatal("om sommeren skal 03:00 i New York være 07:00 UTC")
	}
	if !ny.due(time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC), time.Time{}) || ny.due(time.Date(2026, 12, 1, 7, 59, 0, 0, time.UTC), time.Time{}) {
		t.Fatal("om vinteren skal 03:00 i New York være 08:00 UTC")
	}
	if got := danishWhen(ny.next(time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC), time.Time{}), ny.loc); got != "onsdag 2. december kl. 03:00" {
		t.Fatalf("næste tidspunkt skal vises i den valgte tidszone: %s", got)
	}
	utc, _ := parseSchedule("daily", "03:00", "UTC")
	if !utc.due(time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC), time.Time{}) {
		t.Fatal("UTC skal kunne vælges")
	}
	for _, bad := range []string{"Europe/Kbh", "Local", "local", "../../etc/passwd", "Mars/Olympus"} {
		if _, err := parseSchedule("daily", "03:00", bad); err == nil {
			t.Errorf("%q skal afvises som tidszone", bad)
		}
	}
	for _, z := range updateTZs {
		if _, _, err := loadTZ(z.Zone); err != nil {
			t.Errorf("valget %q (%s) skal være en gyldig tidszone: %v", z.Zone, z.Label, err)
		}
	}
}

func TestUpdaterSavesTimezone(t *testing.T) {
	setVersion(t, "v0.2.0")
	r := newRelease(t, "v0.3.0", "NY", nil)
	u, st := newTestUpdater(t, r)
	if err := u.SetSchedule(true, "sun", "03:00", "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	if got := st.Snapshot().Settings.UpdateTZ; got != "Asia/Tokyo" {
		t.Fatalf("tidszonen skal gemmes: %q", got)
	}
	if err := u.SetSchedule(true, "sun", "03:00", "Europe/Kbh"); err == nil || !strings.Contains(err.Error(), "ukendt tidszone") {
		t.Fatalf("en ukendt tidszone skal afvises med en forklaring: %v", err)
	}
	if st.Snapshot().Settings.UpdateTZ != "Asia/Tokyo" {
		t.Fatal("en afvist ændring må ikke gemmes")
	}
	// søndag 25. oktober kl. 03:00 i Tokyo er lørdag 24. oktober kl. 18:00 UTC.
	u.now = func() time.Time { return time.Date(2026, 10, 24, 17, 0, 0, 0, time.UTC) }
	if v := u.View(); v["TZ"] != "Asia/Tokyo" || v["TZLabel"] != "Japan (Tokyo)" || v["TZOther"] != false || v["Next"] != "søndag 25. oktober kl. 03:00" {
		t.Fatalf("siden skal vise tidszonen og næste tidspunkt i den: %v", v)
	}
	// En tidszone, der ikke er i listen, vises under «Andet …».
	_ = u.SetSchedule(true, "sun", "03:00", "America/Toronto")
	if v := u.View(); v["TZOther"] != true || v["TZLabel"] != "America/Toronto" {
		t.Fatalf("en tidszone uden for listen skal stå under «Andet …»: %v", v)
	}
}

func TestTimezoneFormAndLabels(t *testing.T) {
	if tz, err := tzFromForm("Europe/Oslo", "ignoreres"); err != nil || tz != "Europe/Oslo" {
		t.Fatalf("dropdown'en: %q %v", tz, err)
	}
	if tz, err := tzFromForm("other", " America/Toronto "); err != nil || tz != " America/Toronto " {
		t.Fatalf("«Andet …» skal bruge feltet: %q %v", tz, err)
	}
	if _, err := tzFromForm("other", "  "); err == nil {
		t.Fatal("«Andet …» uden tidszone skal afvises")
	}
	cph, _ := time.LoadLocation("Europe/Copenhagen")
	ny, _ := time.LoadLocation("America/New_York")
	kol, _ := time.LoadLocation("Asia/Kolkata")
	winter, summer := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		loc  *time.Location
		at   time.Time
		want string
	}{{cph, winter, "UTC+1"}, {cph, summer, "UTC+2"}, {ny, winter, "UTC−5"}, {kol, summer, "UTC+5:30"}, {time.UTC, summer, "UTC"}} {
		if got := utcOffset(c.loc, c.at); got != c.want {
			t.Errorf("%s %s: %s, ventede %s", c.loc, c.at.Month(), got, c.want)
		}
	}
	if tzLabel("Europe/Copenhagen") != "Danmark (København)" || tzLabel("America/Toronto") != "America/Toronto" {
		t.Fatal("danske navne fra listen, ellers IANA-navnet")
	}
}

func TestSchemaGuard(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().Settings.Schema != schemaVersion {
		t.Fatal("skemaversionen skal gemmes")
	}
	_ = st.Do(func(db *DB) error { db.Settings.Schema = schemaVersion + 1; db.Settings.AutoUpdate = true; return nil })
	st.Close()
	if _, err := OpenStore(dir, testBox(t)); err == nil || !strings.Contains(err.Error(), "nyere version") {
		t.Fatalf("en ældre version skal nægte at starte på nyere data, fik %v", err)
	}
}

// countRT tæller, hvor mange containere der startes.
type countRT struct {
	*demoRT
	mu     sync.Mutex
	starts int
}

func (c *countRT) Start(inst, image string, cport, hport int, env map[string]string, dir, volume string, logf func(string, ...any)) error {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	return c.demoRT.Start(inst, image, cport, hport, env, dir, volume, logf)
}

func (c *countRT) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}

// Når Vibe Deploy genstarter (fx under en opdatering), overtages kørende apps uden genstart.
func TestReconcileAdoptsRunningApps(t *testing.T) {
	old := drainDelay
	drainDelay = 50 * time.Millisecond
	defer func() { drainDelay = old }()

	dir := t.TempDir()
	st, err := OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	rt := &countRT{demoRT: newDemoRT()}
	m := NewManager(st, rt, dir)
	deployHTML(t, m, "v1")
	_ = m.Approve("bageri")
	before := waitCurrent(t, m, "bageri", 1)
	defer m.stopAll("bageri")
	n := rt.count()

	m2 := NewManager(st, rt, dir) // «ny proces»: containeren kører stadig
	m2.Reconcile()
	after, _ := m2.get("bageri")
	if rt.count() != n || after.HostPort != before.HostPort || after.Slot != before.Slot || after.Status != StatusLive {
		t.Fatalf("en kørende app skal overtages uden genstart: starts %d→%d, %+v", n, rt.count(), after)
	}

	m2.stopAll("bageri") // containeren er væk (fx efter en genstart af maskinen): så startes den
	m2.Reconcile()
	waitCurrent(t, m2, "bageri", 1)
	if rt.count() != n+1 {
		t.Fatalf("en app, der ikke kører, skal startes igen: %d starts", rt.count()-n)
	}
}

func TestHealthz(t *testing.T) {
	setVersion(t, "v0.4.0")
	s := &Server{domain: "min-server.dk"}
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/healthz", nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	var h struct {
		OK      bool
		Version string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if rec.Code != 200 || !h.OK || h.Version != "v0.4.0" {
		t.Fatalf("/healthz på serverens IP: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInstallUpdateUnits(t *testing.T) {
	svc, path := dropHowTo(updateServiceTemplate), dropHowTo(updatePathTemplate)
	if !strings.Contains(svc, "ExecStart=/usr/local/bin/vibedeploy update -data /var/lib/vibedeploy -addr 127.0.0.1:8080") || strings.Contains(svc, "User=") {
		t.Fatalf("opdateringen skal køre som root med de samme stier som tjenesten:\n%s", svc)
	}
	if !strings.Contains(path, "PathExists=/var/lib/vibedeploy/"+updateRequestName) || !strings.Contains(path, "Unit="+updateServiceUnit) {
		t.Fatalf("path-enheden skal se update-request:\n%s", path)
	}
	if strings.Count(renderCaddyfile(installConfig{Domain: "d.dk", Email: "a@b.dk"}), "lb_try_duration 15s") != 3 {
		t.Fatal("Caddy skal vente på Vibe Deploy under en genstart")
	}
}
