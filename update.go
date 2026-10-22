package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- Opdatering af Vibe Deploy selv ----------
//
// To dele med hver sin rettighed:
//
//   - Serveren (bruger vibedeploy, kan ikke skrive i /usr/local/bin) tjekker hver time, om der er en ny,
//     signeret release. Ejeren trykker «Opdater nu», eller den går i gang selv på ejerens tidspunkt (schedule.go). Serveren tager så
//     en backup af state.db og lægger filen update-request i data-mappen. Det er alt, den kan.
//   - systemd ser filen (vibedeploy-update.path) og starter «vibedeploy update» som root
//     (vibedeploy-update.service). Den henter selv den nyeste release, tjekker signatur og SHA-256,
//     skifter binæren ud og genstarter tjenesten. Svarer den nye version ikke, lægges den gamle tilbage
//     (og om nødvendigt også backuppen af state.db).
//
// Serveren kan altså kun bede om en opdatering til en signeret, nyere release, aldrig vælge filen selv.
// Root-delen rører kun faste filnavne direkte i data-mappen og bruger rename/remove, som ikke følger symlinks.

const (
	updateRequestName = "update-request"            // serveren beder om en opdatering
	updateStatusName  = "update-status.json"        // root-delen skriver, hvordan det gik
	updateBackupName  = "update-state-backup.db"    // backup af state.db fra lige før opdateringen
	updatePathUnit    = "vibedeploy-update.path"    // systemd-enheden, der ser update-request
	updateServiceUnit = "vibedeploy-update.service" // systemd-enheden, der kører «vibedeploy update»
)

// updateUnitFile er stien, der viser, at opdateringsdelen er installeret (variabel, så tests kan ændre den).
var updateUnitFile = "/etc/systemd/system/" + updatePathUnit

// UpdateStatus er udfaldet af seneste opdatering, som root-delen skriver det.
type UpdateStatus struct {
	State   string    // running, ok, none, rolledback, failed
	From    string    // version før
	To      string    // version, der blev forsøgt
	Message string    // forklaring på dansk
	Time    time.Time // hvornår status sidst ændrede sig
}

// readUpdateStatus læser update-status.json (tom status, hvis der ikke er nogen).
func readUpdateStatus(dataDir string) UpdateStatus {
	var st UpdateStatus
	if b, err := os.ReadFile(filepath.Join(dataDir, updateStatusName)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// writeFileAtomic skriver en ny fil ved siden af og omdøber den på plads. Opret med O_EXCL og rename
// følger ikke symlinks, så det er sikkert for root at skrive i en mappe, en anden bruger ejer.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ---------- serverens del ----------

// Updater holder øje med nye releases og beder root-delen om at installere dem.
type Updater struct {
	dataDir string
	st      *Store
	client  *http.Client
	base    string
	pub     ed25519.PublicKey
	now     func() time.Time

	mu       sync.Mutex
	latest   *Manifest
	checked  time.Time
	checkErr string
}

// NewUpdater laver en Updater med den indbyggede nøgle og standardadressen.
func NewUpdater(dataDir string, st *Store) *Updater {
	return &Updater{dataDir: dataDir, st: st, client: &http.Client{Timeout: 30 * time.Second},
		base: updateBase(), pub: trustedKey(), now: time.Now}
}

// Installed er sand, når serveren er sat op med install.sh, så root-delen findes.
func (u *Updater) Installed() bool { return fileExists(updateUnitFile) }

// CanUpdate er sand, når en opdatering kan startes herfra: rigtig version, nøgle og root-del.
func (u *Updater) CanUpdate() bool {
	_, ok := parseSemver(version)
	return ok && u.pub != nil && u.Installed()
}

// Check henter den nyeste release og husker den.
func (u *Updater) Check() {
	m, err := fetchRelease(u.client, u.base, u.pub)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = u.now()
	if err != nil {
		u.checkErr = err.Error()
		return
	}
	u.latest, u.checkErr = m, ""
}

// Latest er den nyeste kendte release, hvis den er nyere end den, der kører.
func (u *Updater) Latest() *Manifest {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.latest != nil && newer(u.latest.Version, version) {
		m := *u.latest
		return &m
	}
	return nil
}

// Pending er sand, mens en anmodning venter på root-delen.
func (u *Updater) Pending() bool { return fileExists(filepath.Join(u.dataDir, updateRequestName)) }

// Request tager en backup af state.db og beder root-delen om at opdatere.
func (u *Updater) Request(reason string) error {
	if !u.CanUpdate() {
		return errors.New("opdatering herfra kræver en installation med install.sh og en signeret version")
	}
	if u.Pending() {
		return nil
	}
	backup := filepath.Join(u.dataDir, updateBackupName)
	_ = os.Remove(backup) // VACUUM INTO vil ikke skrive over en fil
	if err := u.st.BackupTo(backup); err != nil {
		return fmt.Errorf("backup af state.db før opdateringen fejlede: %w", err)
	}
	target := ""
	if m := u.Latest(); m != nil {
		target = m.Version
	}
	b, _ := json.Marshal(map[string]any{"from": version, "to": target, "reason": reason, "time": u.now()})
	return writeFileAtomic(filepath.Join(u.dataDir, updateRequestName), b, 0o644)
}

// SetSchedule gemmer, om nye versioner skal installeres automatisk, og hvornår (dag, «HH:MM» og
// tidszone). Tidspunktet gemmes også, når automatik er slået fra, så det er der næste gang.
func (u *Updater) SetSchedule(on bool, day, at, tz string) error {
	sc, err := parseSchedule(day, at, tz)
	if err != nil {
		return err
	}
	return u.st.Do(func(db *DB) error {
		db.Settings.AutoUpdate = on
		db.Settings.UpdateDay, db.Settings.UpdateAt = sc.day, fmt.Sprintf("%02d:%02d", sc.hour, sc.minute)
		db.Settings.UpdateTZ = sc.tz
		return nil
	})
}

// checkEvery er, hvor tit serveren spørger efter en ny release.
const checkEvery = time.Hour

// tick kører hvert minut: spørger efter en ny release, når der er gået en time, og starter den,
// hvis ejeren har slået automatik til, og det valgte tidspunkt er nået.
func (u *Updater) tick() {
	u.mu.Lock()
	stale := u.now().Sub(u.checked) >= checkEvery
	u.mu.Unlock()
	if stale {
		u.Check()
	}
	set := u.st.Snapshot().Settings
	sc := set.schedule()
	if !set.AutoUpdate || !sc.due(u.now(), set.LastAutoTry) || u.Latest() == nil || !u.CanUpdate() {
		return
	}
	// Er en opdatering til netop denne version allerede rullet tilbage, prøves den ikke igen automatisk.
	if st := readUpdateStatus(u.dataDir); st.State == "rolledback" && st.To == u.Latest().Version {
		return
	}
	// Gem forsøget før anmodningen: fejler den, prøves der ikke igen hvert minut, men næste gang.
	now := u.now()
	_ = u.st.Do(func(db *DB) error { db.Settings.LastAutoTry = now; return nil })
	if err := u.Request("automatisk " + sc.describe()); err != nil {
		fmt.Println("Automatisk opdatering:", err)
	}
}

// Run kører tick hvert minut (første gang efter et minut). En udviklingsudgave tjekker ikke.
func (u *Updater) Run() {
	if _, ok := parseSemver(version); !ok {
		return
	}
	for {
		time.Sleep(time.Minute)
		u.tick()
	}
}

// View er det, siden Server viser om Vibe Deploy selv.
func (u *Updater) View() map[string]any {
	u.mu.Lock()
	checked, checkErr := u.checked, u.checkErr
	u.mu.Unlock()
	_, release := parseSemver(version)
	set := u.st.Snapshot().Settings
	sc := set.schedule()
	type dayOpt struct {
		Key, Label string
		Selected   bool
	}
	var days []dayOpt
	for _, d := range updateDays {
		days = append(days, dayOpt{d.Key, d.Label, d.Key == sc.day})
	}
	v := map[string]any{
		"Version": version, "Release": release, "Installed": u.Installed(), "HasKey": u.pub != nil,
		"CanUpdate": u.CanUpdate(), "Pending": u.Pending(), "Auto": set.AutoUpdate,
		"CheckErr": checkErr, "Days": days, "At": fmt.Sprintf("%02d:%02d", sc.hour, sc.minute),
		"Schedule": sc.describe(), "TZ": sc.tz, "TZLabel": tzLabel(sc.tz),
	}
	type tzOpt struct {
		Zone, Label string
		Selected    bool
	}
	var tzs []tzOpt
	inList := false
	for _, z := range updateTZs {
		label := z.Label
		if loc, _, err := loadTZ(z.Zone); err == nil {
			label += " · " + utcOffset(loc, u.now())
		}
		tzs = append(tzs, tzOpt{z.Zone, label, z.Zone == sc.tz})
		inList = inList || z.Zone == sc.tz
	}
	v["TZs"], v["TZOther"] = tzs, !inList
	if set.AutoUpdate {
		if n := sc.next(u.now(), set.LastAutoTry); !n.IsZero() {
			v["Next"] = danishWhen(n, sc.loc)
		}
	}
	if !checked.IsZero() {
		v["Checked"] = relTime(checked)
	}
	if m := u.Latest(); m != nil {
		v["Latest"], v["Notes"] = m.Version, m.Notes
	}
	if st := readUpdateStatus(u.dataDir); st.State != "" {
		v["Status"], v["StatusAgo"] = st, relTime(st.Time)
	}
	return v
}

// ---------- root-delen: «vibedeploy update» ----------

// selfUpdate er alt, «vibedeploy update» bruger. Felterne med funktioner kan erstattes i tests.
type selfUpdate struct {
	dataDir, bin, healthURL string
	client                  *http.Client
	base                    string
	pub                     ed25519.PublicKey
	current                 string                                  // versionen, der kører nu
	systemctl               func(args ...string) error              // styring af vibedeploy.service
	probe                   func(path string) (string, error)       // kører «<binær> version»
	healthy                 func(want string, d time.Duration) bool // svarer tjenesten med versionen want?
	log                     io.Writer
}

// runUpdate er «vibedeploy update». Den startes af systemd som root, når serveren har lagt update-request.
func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	dataDir := fs.String("data", "/var/lib/vibedeploy", "Vibe Deploys data-mappe")
	addr := fs.String("addr", "127.0.0.1:8080", "adressen, tjenesten lytter på")
	bin := fs.String("bin", binaryPath, "den installerede binær")
	check := fs.Bool("check", false, "vis kun, om der er en ny version")
	fs.Parse(args)

	u := &selfUpdate{dataDir: *dataDir, bin: *bin, healthURL: "http://" + *addr + "/healthz",
		client: &http.Client{Timeout: 5 * time.Minute}, base: updateBase(), pub: trustedKey(), current: version,
		systemctl: func(a ...string) error { return run("systemctl", a...) }, probe: probeVersion, log: os.Stdout}
	u.healthy = u.httpHealthy
	if *check {
		m, err := fetchRelease(u.client, u.base, u.pub)
		if err != nil {
			fatalf("%v", err)
		}
		if newer(m.Version, version) {
			fmt.Printf("Ny version: %s (du har %s)\n", m.Version, version)
		} else {
			fmt.Printf("Du har den nyeste version (%s)\n", version)
		}
		return
	}
	if os.Geteuid() != 0 {
		fatalf("kør som root (systemd gør det: systemctl start %s)", updateServiceUnit)
	}
	if err := u.run(); err != nil {
		fatalf("%v", err)
	}
}

// status skriver update-status.json og logger beskeden.
func (u *selfUpdate) status(state, to, msg string) {
	fmt.Fprintf(u.log, "==> %s\n", msg)
	b, _ := json.MarshalIndent(UpdateStatus{State: state, From: u.current, To: to, Message: msg, Time: time.Now()}, "", "  ")
	if err := writeFileAtomic(filepath.Join(u.dataDir, updateStatusName), b, 0o644); err != nil {
		fmt.Fprintf(u.log, "Kunne ikke skrive status: %v\n", err)
	}
}

// run henter, tjekker og installerer den nyeste release og ruller tilbage, hvis den ikke svarer.
func (u *selfUpdate) run() error {
	_ = os.Remove(filepath.Join(u.dataDir, updateRequestName))
	u.status("running", "", "Leder efter en ny version …")

	m, err := fetchRelease(u.client, u.base, u.pub)
	if err != nil {
		u.status("failed", "", "Kunne ikke hente releasen: "+err.Error())
		return err
	}
	if !newer(m.Version, u.current) {
		u.status("none", m.Version, fmt.Sprintf("Du har allerede den nyeste version (%s).", u.current))
		return nil
	}
	to := m.Version
	u.status("running", to, fmt.Sprintf("Henter %s …", to))
	newBin := u.bin + ".new"
	if err := downloadVerified(u.client, u.base, m, binaryName(), newBin); err != nil {
		u.status("failed", to, "Den nye version blev ikke installeret: "+err.Error())
		return err
	}
	defer os.Remove(newBin)
	if got, err := u.probe(newBin); err != nil || got != to {
		msg := fmt.Sprintf("Den nye binær kunne ikke starte eller er ikke %s (%q, %v); intet er ændret.", to, got, err)
		u.status("failed", to, msg)
		return errors.New(msg)
	}

	u.status("running", to, fmt.Sprintf("Installerer %s og genstarter Vibe Deploy. Appene kører videre imens.", to))
	if err := u.systemctl("stop", "vibedeploy"); err != nil {
		u.status("failed", to, "Tjenesten kunne ikke stoppes: "+err.Error())
		return err
	}
	prev := u.bin + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(u.bin, prev); err != nil { // behold den gamle; hård kobling, så binæren aldrig mangler
		_ = u.systemctl("start", "vibedeploy")
		u.status("failed", to, "Kunne ikke gemme den gamle version: "+err.Error())
		return err
	}
	if err := os.Rename(newBin, u.bin); err != nil {
		_ = u.systemctl("start", "vibedeploy")
		u.status("failed", to, "Kunne ikke lægge den nye version på plads: "+err.Error())
		return err
	}
	startErr := u.systemctl("start", "vibedeploy")
	if startErr == nil && u.healthy(to, 60*time.Second) {
		u.status("ok", to, fmt.Sprintf("Opdateret fra %s til %s.", u.current, to))
		return nil
	}
	return u.rollback(to, prev, startErr)
}

// rollback lægger den gamle binær tilbage. Starter den ikke på de nuværende data (fordi den nye version
// har ændret skemaet), lægges backuppen af state.db fra lige før opdateringen også tilbage.
func (u *selfUpdate) rollback(to, prev string, startErr error) error {
	reason := fmt.Sprintf("%s svarede ikke inden for 60 sekunder", to)
	if startErr != nil {
		reason = fmt.Sprintf("%s kunne ikke starte (%v)", to, startErr)
	}
	fmt.Fprintf(u.log, "==> %s; ruller tilbage til %s\n", reason, u.current)
	_ = u.systemctl("stop", "vibedeploy")
	_ = os.Remove(u.bin + ".failed")
	_ = os.Rename(u.bin, u.bin+".failed")
	if err := os.Rename(prev, u.bin); err != nil {
		msg := fmt.Sprintf("%s. Den gamle version kunne IKKE lægges tilbage (%v); læg %s på plads som %s i hånden.", reason, err, prev, u.bin)
		u.status("failed", to, msg)
		return errors.New(msg)
	}
	if u.systemctl("start", "vibedeploy") == nil && u.healthy(u.current, 60*time.Second) {
		msg := fmt.Sprintf("%s, så %s kører igen. Ingen data er rørt. Den fejlede version ligger i %s.failed.", reason, u.current, u.bin)
		u.status("rolledback", to, msg)
		return errors.New(msg)
	}
	backup := filepath.Join(u.dataDir, updateBackupName)
	if !fileExists(backup) {
		msg := fmt.Sprintf("%s, og %s starter heller ikke igen. Se journalctl -u vibedeploy.", reason, u.current)
		u.status("failed", to, msg)
		return errors.New(msg)
	}
	_ = u.systemctl("stop", "vibedeploy")
	db := filepath.Join(u.dataDir, "state.db")
	_ = os.Remove(db + "-wal")
	_ = os.Remove(db + "-shm")
	_ = os.Remove(db + ".efter-opdatering")
	_ = os.Rename(db, db+".efter-opdatering")
	if err := os.Rename(backup, db); err != nil {
		msg := fmt.Sprintf("%s, og backuppen af state.db kunne ikke lægges tilbage: %v", reason, err)
		u.status("failed", to, msg)
		return errors.New(msg)
	}
	if u.systemctl("start", "vibedeploy") == nil && u.healthy(u.current, 60*time.Second) {
		msg := fmt.Sprintf("%s, så %s kører igen med data fra lige før opdateringen. Ændringer siden da ligger i state.db.efter-opdatering.", reason, u.current)
		u.status("rolledback", to, msg)
		return errors.New(msg)
	}
	msg := fmt.Sprintf("%s, og %s starter heller ikke med backuppen. Se journalctl -u vibedeploy.", reason, u.current)
	u.status("failed", to, msg)
	return errors.New(msg)
}

// httpHealthy venter, til tjenesten svarer på /healthz med versionen want.
func (u *selfUpdate) httpHealthy(want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	c := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if resp, err := c.Get(u.healthURL); err == nil {
			var h struct {
				OK      bool   `json:"ok"`
				Version string `json:"version"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && h.OK && h.Version == want {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// probeVersion kører «<binær> version» og returnerer det, den skriver.
func probeVersion(path string) (string, error) {
	out, err := exec.Command(path, "version").Output()
	return strings.TrimSpace(string(out)), err
}

// healthz svarer, at serveren kører, og med hvilken version. Root-delen bruger den efter en opdatering.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": version})
}
