package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Manager styrer appenes livsforløb: udgivelse, bygning, start/stop, rollback, variabler,
// data og backups. Hver app har sin egen lås (lockFor), så to operationer på samme app
// aldrig kører samtidig.
type Manager struct {
	st      *Store
	rt      Runtime
	dataDir string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewManager laver en Manager, der gemmer filer under dataDir.
func NewManager(st *Store, rt Runtime, dataDir string) *Manager {
	return &Manager{st: st, rt: rt, dataDir: dataDir, locks: map[string]*sync.Mutex{}}
}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
var reserved = map[string]bool{"admin": true, "www": true, "mcp": true, "static": true, "api": true}
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var secretish = regexp.MustCompile(`(?i)(secret|token|password|passwd|api_?key|private|_key$|^key$|kode)`)

// slugify gør et appnavn til en adresse-venlig slug: små bogstaver, æøå omskrevet, andre tegn
// bliver til bindestreger, højst 32 tegn. «Min Bageri-app» → «min-bageri-app».
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("æ", "ae", "ø", "oe", "å", "aa", "é", "e", "ü", "u", "ö", "o", "ä", "a").Replace(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	return out
}

// lockFor returnerer appens egen lås (oprettes ved første brug).
func (m *Manager) lockFor(slug string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[slug] == nil {
		m.locks[slug] = &sync.Mutex{}
	}
	return m.locks[slug]
}

// logPath er stien til appens bygge- og deploylog.
func (m *Manager) logPath(slug string) string {
	return filepath.Join(m.dataDir, "logs", slug+".log")
}

// logger returnerer en log-funktion for appen. Den skriver med tidsstempel og roterer filen
// (én gammel kopi, .1), når den bliver større end maxLogSize.
func (m *Manager) logger(slug string) func(string, ...any) {
	return func(f string, a ...any) {
		_ = os.MkdirAll(filepath.Dir(m.logPath(slug)), 0o700)
		if st, err := os.Stat(m.logPath(slug)); err == nil && st.Size() > maxLogSize {
			_ = os.Rename(m.logPath(slug), m.logPath(slug)+".1") // behold én gammel fil
		}
		fh, err := os.OpenFile(m.logPath(slug), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer fh.Close()
		fmt.Fprintf(fh, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
	}
}

// Logs returnerer de seneste linjer: bygge- og deploylog plus appens egen log.
func (m *Manager) Logs(slug string, lines int) string {
	b, _ := os.ReadFile(m.logPath(slug))
	l := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(l) > lines {
		l = l[len(l)-lines:]
	}
	out := strings.Join(l, "\n")
	a, _ := m.get(slug)
	if rl := strings.TrimSpace(m.rt.Logs(instName(slug, a.Slot), lines)); rl != "" {
		out += "\n\n--- appens egen log ---\n" + rl
	}
	return strings.TrimSpace(out)
}

// get returnerer en dyb kopi af appen, så kalderen kan læse den uden lås.
func (m *Manager) get(slug string) (App, bool) {
	var out App
	found := false
	m.st.View(func(db *DB) {
		if a, ok := db.Apps[slug]; ok {
			b, _ := json.Marshal(a)
			_ = json.Unmarshal(b, &out)
			found = true
		}
	})
	return out, found
}

// update ændrer appen under lås, gemmer og sætter Updated. Findes appen ikke, sker der intet.
func (m *Manager) update(slug string, f func(a *App)) {
	_ = m.st.Do(func(db *DB) error {
		if a, ok := db.Apps[slug]; ok {
			f(a)
			a.Updated = time.Now()
		}
		return nil
	})
}

// setStatus sætter status og den forklarende tekst, der vises i dashboardet og til AI'en.
func (m *Manager) setStatus(slug, status, text string) {
	m.update(slug, func(a *App) { a.Status, a.StatusText = status, text })
}

// ---------- Udgivelse ----------

// DeployResult beskriver udfaldet af en udgivelse: den nye version, og om ejeren skal godkende først.
type DeployResult struct {
	App      App
	NeedsOK  bool
	NewApp   bool
	VersionN int
	Warnings []string // mulige fejl i Dockerfile, som AI'en får at vide (dockerfileWarnings)
}

// Deploy udgiver en app ud fra filer sendt som tekst (små apps, fx fra claude.ai).
func (m *Manager) Deploy(name string, port int, files map[string]string, note, ownerID string) (*DeployResult, error) {
	if len(files) == 0 || len(files) > 300 {
		return nil, errors.New("send mellem 1 og 300 filer; brug prepare_upload til større apps")
	}
	total := 0
	for p, c := range files {
		if err := checkPath(p); err != nil {
			return nil, err
		}
		total += len(c)
	}
	if total > 8<<20 {
		return nil, errors.New("filerne fylder for meget (maks 8 MB tekst); brug prepare_upload")
	}
	return m.deployWith(name, port, note, ownerID, fmt.Sprintf("%d filer som tekst", len(files)), "", func(dir string) error {
		return writeFiles(dir, files)
	})
}

// DeployArchive udgiver en app ud fra en pakket mappe (tar.gz), sendt til en upload-adresse.
func (m *Manager) DeployArchive(name string, port int, note, ownerID string, archive io.Reader) (*DeployResult, error) {
	var count int
	var skipped []string
	res, err := m.deployWith(name, port, note, ownerID, "pakket mappe", "", func(dir string) error {
		var err error
		count, skipped, err = extractTarGz(archive, dir, maxUnpacked, maxFiles)
		return err
	})
	if err == nil {
		logf := m.logger(res.App.Slug)
		logf("Pakke udpakket: %d filer", count)
		if len(skipped) > 0 {
			logf("Sprunget over (symlinks o.l.): %s", strings.Join(skipped, ", "))
		}
	}
	return res, err
}

// deployWith er fælles for alle veje: valider, opret version, læg filer, start bygning eller vent på godkendelse.
// from er et færdigt image, der hentes i stedet for at bygge (tom = byg fra Dockerfile i mappen).
func (m *Manager) deployWith(name string, port int, note, ownerID, how, from string, fill func(dir string) error) (*DeployResult, error) {
	slug := slugify(name)
	if !slugRe.MatchString(slug) || reserved[slug] {
		return nil, fmt.Errorf("navnet «%s» kan ikke bruges som adresse; vælg et andet, fx «min-bageri-app»", name)
	}
	if port <= 0 || port > 65535 {
		port = 8080
	}
	if existing, ok := m.get(slug); ok && existing.OwnerID != ownerID && ownerID != "" {
		// Ejerskab tjekkes også af kalderen; dette er et ekstra værn.
		var owner *User
		m.st.View(func(db *DB) { owner = db.user(ownerID) })
		if owner == nil || !owner.IsAdmin() {
			return nil, fmt.Errorf("navnet «%s» er optaget; vælg et andet navn", name)
		}
	}

	a0, exists := m.get(slug)
	n := 1
	if exists {
		n = len(a0.Versions) + 1
	}
	dir := filepath.Join(m.dataDir, "apps", slug, fmt.Sprintf("v%d", n))
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := fill(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if m.rt.Name() == "docker" && from == "" {
		if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
			_ = os.RemoveAll(dir)
			return nil, errors.New("der mangler en Dockerfile i roden; tilføj en, der starter appen og lytter på den port, du angiver (se get_build_guide)")
		}
	}
	var warnings []string
	if from == "" {
		warnings = dockerfileWarnings(dir, port)
	}

	hostPort, err := freePort()
	if err != nil {
		return nil, err
	}
	res := &DeployResult{Warnings: warnings}
	err = m.st.Do(func(db *DB) error {
		a, ok := db.Apps[slug]
		if !ok {
			a = &App{Slug: slug, Name: strings.TrimSpace(name), OwnerID: ownerID, Visibility: VisPrivate, HostPort: hostPort, Created: time.Now()}
			db.Apps[slug] = a
			res.NewApp = true
		}
		if len(a.Versions)+1 != n { // en anden udgivelse kom først
			return errors.New("en anden udgivelse er i gang for denne app; prøv igen om lidt")
		}
		a.Port = port
		a.Versions = append(a.Versions, Version{N: n, Note: strings.TrimSpace(note), Created: time.Now(), Dir: dir, From: from})
		a.FromImage = from
		if from != "" {
			a.Git = nil
		}
		a.Pending = n
		a.Updated = time.Now()
		res.VersionN = n
		if !a.Approved {
			a.Status, a.StatusText = StatusWaiting, "Venter på din godkendelse"
			res.NeedsOK = true
		} else {
			a.Status, a.StatusText = StatusBuilding, "Pakker din app …"
		}
		res.App = *a
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m.logger(slug)("Version %d modtaget fra AI (%s)", n, how)
	for _, w := range warnings {
		m.logger(slug)("Advarsel: %s", w)
	}
	if !res.NeedsOK {
		go m.pipeline(slug, n)
	}
	return res, nil
}

// checkPath afviser filstier, der er tomme, absolutte eller kan nå uden for appens mappe
// («..», «.», backslash, NUL).
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("ugyldig filsti: %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "" || part == "." {
			return fmt.Errorf("ugyldig filsti: %q", p)
		}
	}
	return nil
}

// writeFiles skriver filerne (sti → indhold) ind i dir og tjekker, at hver sti bliver inden for dir.
func writeFiles(dir string, files map[string]string) error {
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if !strings.HasPrefix(full, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("ugyldig filsti: %q", p)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Approve godkender en ny app og starter bygningen af den ventende version.
// Efter første godkendelse bygges nye versioner automatisk.
func (m *Manager) Approve(slug string) error {
	a, ok := m.get(slug)
	if !ok || a.Status != StatusWaiting {
		return errors.New("appen venter ikke på godkendelse")
	}
	m.update(slug, func(a *App) {
		a.Approved = true
		a.Status, a.StatusText = StatusBuilding, "Pakker din app …"
	})
	go m.pipeline(slug, a.Pending)
	return nil
}

// Reject afviser en ny app, der venter på godkendelse, og sletter den.
func (m *Manager) Reject(slug string) error {
	a, ok := m.get(slug)
	if !ok || a.Status != StatusWaiting {
		return errors.New("appen venter ikke på godkendelse")
	}
	return m.st.Do(func(db *DB) error {
		delete(db.Apps, slug)
		return nil
	})
}

// pipeline bygger version n og skifter over til den. Fejler den, kører den gamle videre.
func (m *Manager) pipeline(slug string, n int) {
	l := m.lockFor(slug)
	l.Lock()
	defer l.Unlock()
	logf := m.logger(slug)
	a, ok := m.get(slug)
	if !ok || n < 1 || n > len(a.Versions) {
		return
	}
	ver := a.Versions[n-1]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var image string
	var err error
	if ver.From != "" {
		m.setStatus(slug, StatusBuilding, "Henter din app …")
		logf("== Henter version %d ==", n)
		ref, _ := parseImage(ver.From) // valideret ved udgivelsen
		user, token := registryLogin(a)
		image, err = m.rt.Pull(ctx, slug, n, ref, user, token, logf)
	} else {
		m.setStatus(slug, StatusBuilding, "Bygger din app …")
		logf("== Bygger version %d ==", n)
		image, err = m.rt.Build(ctx, slug, n, ver.Dir, logf)
	}
	if err != nil {
		logf("FEJL: %v", err)
		m.failed(slug, n, a.Current)
		return
	}
	m.update(slug, func(a *App) { a.Versions[n-1].Image = image })

	if _, err := m.Backup(slug, fmt.Sprintf("før version %d", n)); err != nil {
		logf("Backup før ny version fejlede: %v", err)
	}
	m.setStatus(slug, StatusStarting, "Starter din app …")
	if err := m.startVersion(slug, n, image); err != nil {
		logf("FEJL: %v", err)
		m.failed(slug, n, a.Current)
		return
	}
	m.update(slug, func(a *App) {
		a.Current, a.NeedsRestart = n, false
		a.Status, a.StatusText = StatusLive, ""
	})
	logf("== Version %d er live ==", n)
	m.pruneVersions(slug)
}

// failed markerer version n som fejlet. Den nye version blev startet ved siden af den gamle,
// så kører den forrige version (prev) stadig, er der intet at gøre: appen var online hele tiden.
// Kører den ikke (fx fordi appen var sat på pause), startes den igen; ellers står appen som fejlet.
func (m *Manager) failed(slug string, n, prev int) {
	logf := m.logger(slug)
	text := explain(m.Logs(slug, 60))
	m.update(slug, func(a *App) { a.Versions[n-1].Failed = true })
	if prev > 0 {
		a, _ := m.get(slug)
		if a.HostPort > 0 && waitHealthy(a.HostPort, 2*time.Second) {
			logf("Version %d fejlede; version %d kørte videre uden afbrydelse", n, prev)
			m.setStatus(slug, StatusLive, fmt.Sprintf("Version %d fejlede, så version %d kører stadig. %s", n, prev, text))
			return
		}
		if img := a.Versions[prev-1].Image; img != "" {
			if err := m.startVersion(slug, prev, img); err == nil {
				logf("Version %d fejlede; version %d kører videre", n, prev)
				m.setStatus(slug, StatusLive, fmt.Sprintf("Version %d fejlede, så version %d kører stadig. %s", n, prev, text))
				return
			}
		}
	}
	m.setStatus(slug, StatusFailed, text)
}

// drainDelay er, hvor længe den gamle container får lov at køre efter skiftet, så forespørgsler,
// der allerede var i gang, kan blive færdige. Derefter får den SIGTERM (se Runtime.Stop).
var drainDelay = 10 * time.Second

// startVersion udruller version n uden nedetid ("blue-green"):
//
//  1. Den nye container startes på appens ledige plads (otherSlot) og en ny port, ved siden af den gamle.
//  2. Serveren venter op til 40 sekunder på, at den svarer på /health.
//  3. Trafikken flyttes i ét skridt: proxyen læser HostPort ved hver forespørgsel.
//  4. Den gamle container lukkes, når drainDelay er gået (retire).
//
// Svarer den nye version ikke, fjernes den igen, og den gamle kører uforstyrret videre.
// Under overlappet deler de to containere appens /data-mappe.
// Kaldes med appens lås (lockFor) holdt; tager selv pladslåsen (slotLock).
func (m *Manager) startVersion(slug string, n int, image string) error {
	a, ok := m.get(slug)
	if !ok {
		return errors.New("appen findes ikke")
	}
	logf := m.logger(slug)
	vol, err := m.ensureVolume(slug)
	if err != nil {
		return err
	}
	// Pladslåsen dækker start → skift, så retire aldrig lukker en container, der er ved at blive startet.
	// (Appens lås kan ikke bruges til det: den holdes under hele den næste bygning.)
	sl := m.slotLock(slug)
	sl.Lock()
	defer sl.Unlock()
	slot := otherSlot(a.Slot)
	inst := instName(slug, slot)
	port, err := freePort()
	if err != nil {
		return err
	}
	if err := m.rt.Start(inst, image, a.Port, port, envFor(a), a.Versions[n-1].Dir, vol, logf); err != nil {
		_ = m.rt.Stop(inst)
		return err
	}
	if !waitHealthy(port, 40*time.Second) {
		_ = m.rt.Stop(inst)
		return fmt.Errorf("appen svarer ikke på port %d inden for 40 sekunder – lytter den på den port, du har angivet?", a.Port)
	}
	old, delay := a.Slot, drainDelay
	m.update(slug, func(a *App) { a.HostPort, a.Slot = port, slot })
	logf("Trafikken går nu til version %d (%s); den forrige container lukkes om %s", n, inst, delay)
	go m.retire(slug, old, delay)
	return nil
}

// retire lukker containeren på pladsen slot, når delay er gået. Under pladslåsen tjekkes,
// at pladsen stadig ikke er den aktive: en ny udrulning kan i mellemtiden have skiftet til den.
func (m *Manager) retire(slug, slot string, delay time.Duration) {
	time.Sleep(delay)
	sl := m.slotLock(slug)
	sl.Lock()
	defer sl.Unlock()
	if a, ok := m.get(slug); ok && a.Slot == slot {
		return
	}
	_ = m.rt.Stop(instName(slug, slot))
}

// slotLock er appens lås for start og lukning af containere på dens to pladser.
// Nøglen kan ikke forveksles med en app, da slugs ikke må indeholde «#».
func (m *Manager) slotLock(slug string) *sync.Mutex { return m.lockFor(slug + "#slot") }

// stopAll lukker begge appens pladser.
func (m *Manager) stopAll(slug string) {
	_ = m.rt.Stop(instName(slug, ""))
	_ = m.rt.Stop(instName(slug, "b"))
}

// envFor samler miljøvariablerne til containeren: PORT og DATA_DIR, appens variabler og de
// udfyldte hemmeligheder (undtagen login til registryet). Hemmeligheder vinder ved navnesammenfald.
func envFor(a App) map[string]string {
	env := map[string]string{"PORT": fmt.Sprint(a.Port), "DATA_DIR": "/data"}
	for _, kv := range a.Env {
		env[kv.Key] = kv.Value
	}
	for _, kv := range a.Secrets {
		if kv.Value != "" && !isRegistryKey(kv.Key) { // registry-login er kun til serveren
			env[kv.Key] = kv.Value
		}
	}
	return env
}

// explain oversætter kendte fejl i loggen til en kort forklaring på dansk til dashboardet.
func explain(log string) string {
	l := strings.ToLower(log)
	switch {
	case strings.Contains(l, errRegistryAuth.Error()):
		return "Imaget findes ikke, eller login mangler eller er forkert. Er det privat, så bed Claude udgive det igen som privat image, og tjek " + RegistryUserKey + " og " + RegistryTokenKey + " under Indstillinger."
	case strings.Contains(l, "manifest unknown"):
		return "Den version (tag) af imaget findes ikke. Bed Claude tjekke navnet."
	case strings.Contains(l, "no matching manifest"):
		return "Imaget findes ikke til serverens processortype. Det skal bygges til linux/amd64 (eller den type, serveren har)."
	case strings.Contains(l, "cannot connect to the docker daemon"):
		return "Docker kører ikke på serveren. Start Docker, og prøv igen."
	case strings.Contains(l, "port is already allocated"):
		return "Adressen var optaget. Genstart appen, så vælger serveren en ny."
	case strings.Contains(l, "npm err"):
		return "Appens pakker kunne ikke installeres. Bed Claude rette afhængighederne."
	case strings.Contains(l, "no such file or directory") || strings.Contains(l, "not found"):
		return "Byggeopskriften peger på en fil, der ikke findes. Bed Claude tjekke Dockerfile."
	case strings.Contains(l, "svarer ikke"):
		return "Appen startede, men svarede ikke. Den lytter måske på en anden port end angivet, eller kun på localhost i stedet for 0.0.0.0."
	}
	return "Appen kunne ikke bygges eller starte. Bed Claude læse loggen og rette det."
}

// ---------- Styring ----------

// Restart starter den nuværende version igen, fx så ændrede variabler træder i kraft.
func (m *Manager) Restart(slug string) error {
	a, ok := m.get(slug)
	if !ok {
		return errors.New("appen findes ikke")
	}
	if a.Current == 0 {
		return errors.New("der er ingen version at genstarte endnu")
	}
	return m.switchTo(slug, a.Current)
}

// Rollback skifter til en tidligere version, som stadig har et image (ikke ryddet op).
func (m *Manager) Rollback(slug string, n int) error {
	a, ok := m.get(slug)
	if !ok {
		return errors.New("appen findes ikke")
	}
	if n < 1 || n > len(a.Versions) || a.Versions[n-1].Image == "" {
		return errors.New("den version kan ikke bruges (den blev aldrig bygget færdigt eller er ryddet op)")
	}
	return m.switchTo(slug, n)
}

// switchTo starter version n i baggrunden og opdaterer status, når den kører (eller fejler).
// Den returnerer med det samme.
func (m *Manager) switchTo(slug string, n int) error {
	a, _ := m.get(slug)
	img := a.Versions[n-1].Image
	m.setStatus(slug, StatusStarting, "Starter din app …")
	go func() {
		l := m.lockFor(slug)
		l.Lock()
		defer l.Unlock()
		if err := m.startVersion(slug, n, img); err != nil {
			m.logger(slug)("FEJL: %v", err)
			m.setStatus(slug, StatusFailed, explain(err.Error()))
			return
		}
		m.update(slug, func(a *App) { a.Current, a.NeedsRestart = n, false; a.Status, a.StatusText = StatusLive, "" })
		m.logger(slug)("Version %d kører", n)
	}()
	return nil
}

// Stop sætter appen på pause. Versioner og data bevares.
func (m *Manager) Stop(slug string) error {
	if _, ok := m.get(slug); !ok {
		return errors.New("appen findes ikke")
	}
	m.stopAll(slug)
	m.setStatus(slug, StatusStopped, "Appen er sat på pause")
	m.logger(slug)("Appen blev stoppet")
	return nil
}

// Delete fjerner appen helt: container, versioner, data, backups, log og deploy key.
func (m *Manager) Delete(slug string) error {
	m.stopAll(slug)
	_ = os.RemoveAll(filepath.Join(m.dataDir, "apps", slug))
	_ = os.RemoveAll(m.volumeDir(slug))
	_ = os.RemoveAll(m.backupDir(slug))
	_ = os.Remove(m.logPath(slug))
	_ = os.Remove(m.logPath(slug) + ".1")
	m.RemoveKey(slug)
	return m.st.Do(func(db *DB) error { delete(db.Apps, slug); return nil })
}

// ---------- Variabler og hemmeligheder ----------

// SetEnv erstatter appens almindelige variabler (fra dashboardet).
// Ændringen træder i kraft ved næste genstart.
func (m *Manager) SetEnv(slug string, vars []KV) error {
	for _, kv := range vars {
		if !envKeyRe.MatchString(kv.Key) {
			return fmt.Errorf("«%s» er ikke et gyldigt variabelnavn (brug bogstaver, tal og _)", kv.Key)
		}
		if strings.ContainsAny(kv.Value, "\r\n") {
			return fmt.Errorf("værdien for %s må ikke indeholde linjeskift", kv.Key)
		}
	}
	return m.st.Do(func(db *DB) error {
		a, ok := db.Apps[slug]
		if !ok {
			return errors.New("appen findes ikke")
		}
		a.Env = vars
		a.NeedsRestart, a.Updated = true, time.Now()
		return nil
	})
}

// MergeEnv bruges af AI'en: tilføjer/ændrer variabler, men afviser det, der ligner hemmeligheder.
func (m *Manager) MergeEnv(slug string, vars map[string]string) error {
	a, ok := m.get(slug)
	if !ok {
		return errors.New("appen findes ikke")
	}
	for k := range vars {
		if secretish.MatchString(k) {
			return fmt.Errorf("%s ligner en hemmelighed. Brug request_secret, så ejeren selv indsætter den i dashboardet", k)
		}
	}
	merged := map[string]string{}
	var order []string
	for _, kv := range a.Env {
		merged[kv.Key] = kv.Value
		order = append(order, kv.Key)
	}
	for k, v := range vars {
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = v
	}
	var out []KV
	for _, k := range order {
		out = append(out, KV{k, merged[k]})
	}
	return m.SetEnv(slug, out)
}

// SetSecret gemmer værdien af en hemmelighed. Kaldes kun fra dashboardet af ejeren;
// AI'en kan aldrig sætte eller læse værdien.
func (m *Manager) SetSecret(slug, key, value string) error {
	if !envKeyRe.MatchString(key) {
		return fmt.Errorf("«%s» er ikke et gyldigt navn", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("hemmeligheden må ikke indeholde linjeskift")
	}
	return m.st.Do(func(db *DB) error {
		a, ok := db.Apps[slug]
		if !ok {
			return errors.New("appen findes ikke")
		}
		for i := range a.Secrets {
			if a.Secrets[i].Key == key {
				a.Secrets[i].Value = value
				a.NeedsRestart, a.Updated = true, time.Now()
				return nil
			}
		}
		a.Secrets = append(a.Secrets, KV{key, value})
		a.NeedsRestart, a.Updated = true, time.Now()
		return nil
	})
}

// RequestSecrets opretter tomme pladser, som ejeren udfylder i dashboardet.
func (m *Manager) RequestSecrets(slug string, keys []string) error {
	for _, k := range keys {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("«%s» er ikke et gyldigt navn", k)
		}
	}
	return m.st.Do(func(db *DB) error {
		a, ok := db.Apps[slug]
		if !ok {
			return errors.New("appen findes ikke")
		}
		for _, k := range keys {
			found := false
			for _, s := range a.Secrets {
				if s.Key == k {
					found = true
				}
			}
			if !found {
				a.Secrets = append(a.Secrets, KV{k, ""})
			}
		}
		return nil
	})
}

// Reconcile bringer apps i orden, når Vibe Deploy starter. Kører en apps container stadig (Docker holder
// den i gang, fx mens Vibe Deploy opdateres eller genstartes), overtages den, som den er, uden genstart;
// kun den ubrugte plads ryddes op. Ellers startes appens nuværende version igen. Hver app tjekkes for sig,
// så en app, der ikke svarer, ikke forsinker de andre.
func (m *Manager) Reconcile() {
	db := m.st.Snapshot()
	var wg sync.WaitGroup
	for slug, a := range db.Apps {
		switch {
		case a.Current > 0 && (a.Status == StatusLive || a.Status == StatusStarting || a.Status == StatusBuilding):
			wg.Add(1)
			go func(slug string, a App) {
				defer wg.Done()
				m.reconcileApp(slug, a)
			}(slug, *a)
		case a.Status == StatusBuilding || a.Status == StatusStarting:
			m.setStatus(slug, StatusFailed, "Serveren blev lukket, mens appen blev bygget. Bed Claude udgive den igen.")
		}
	}
	wg.Wait()
}

// reconcileApp overtager appens kørende container eller starter den igen.
func (m *Manager) reconcileApp(slug string, a App) {
	logf := m.logger(slug)
	if a.HostPort > 0 && waitHealthy(a.HostPort, 2*time.Second) {
		text := ""
		if a.Status != StatusLive {
			text = fmt.Sprintf("Serveren blev genstartet, mens en ny version blev bygget; version %d kører stadig. Bed Claude udgive igen.", a.Current)
		}
		m.setStatus(slug, StatusLive, text)
		logf("Vibe Deploy startede; version %d kørte allerede og fortsætter uden genstart", a.Current)
		go func() { // en container fra en afbrudt udrulning på den anden plads
			sl := m.slotLock(slug)
			sl.Lock()
			defer sl.Unlock()
			if cur, ok := m.get(slug); ok && cur.Slot == a.Slot {
				_ = m.rt.Stop(instName(slug, otherSlot(a.Slot)))
			}
		}()
		return
	}
	m.setStatus(slug, StatusStarting, "Starter din app …")
	_ = m.switchTo(slug, a.Current)
}
