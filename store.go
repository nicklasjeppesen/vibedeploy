package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Status for en app.
const (
	StatusWaiting  = "waiting"  // venter på ejerens godkendelse
	StatusBuilding = "building" // bygger image
	StatusStarting = "starting" // starter container
	StatusLive     = "live"
	StatusStopped  = "stopped"
	StatusFailed   = "failed"
)

// User er en konto på serveren. Adgangskoden gemmes kun som PBKDF2-hash med salt.
type User struct {
	ID, Email, Name, Salt, Hash string
	Role                        string // "owner" (administrator) eller "member"
	Builder                     bool   // må forbinde en AI og udgive egne apps
	Created                     time.Time
}

// Session er enten en dashboard-session (AppSlug tom) eller en login til én bestemt app.
type Session struct {
	UserID  string
	AppSlug string
	Expires time.Time
}

// Invite er et invitationslink, ejeren har oprettet. Builder afgør, om den nye konto må bygge apps.
type Invite struct {
	Builder          bool
	Created, Expires time.Time
}

// Ticket er en engangsbillet, der flytter et login fra dashboardet til en apps adresse.
type Ticket struct {
	UserID, Slug string
	Expires      time.Time
}

// OAuthClient er et AI-værktøj, der har registreret sig selv via /register.
type OAuthClient struct {
	Name         string
	RedirectURIs []string
	Created      time.Time
}

// AuthCode er en kortlivet OAuth-kode, der byttes til et Token. Challenge er PKCE-værdien (S256).
type AuthCode struct {
	ClientID, UserID, RedirectURI, Challenge string
	Expires                                  time.Time
}

// Token er en adgang for et AI-værktøj til /mcp: enten fra OAuth eller en personlig adgangsnøgle (pat).
type Token struct {
	UserID, ClientID, Kind string // Kind: "oauth" eller "pat"
	Label                  string
	Created, Expires       time.Time
}

// Version er én udgivelse af en app. N starter ved 1 og svarer til pladsen i App.Versions (N-1).
type Version struct {
	N       int
	Note    string
	Created time.Time
	Image   string // sat når bygningen er lykkedes
	Dir     string
	From    string // færdigt image, der hentes i stedet for at bygge (deploy_image)
	Failed  bool
	Pruned  bool // filer og image er ryddet op; kan ikke bruges til rollback
}

// Upload er en engangsadresse, som AI'en kan sende en pakket mappe til.
type Upload struct {
	UserID, Name, Note string
	Port               int
	Expires            time.Time
}

// KV er en miljøvariabel eller hemmelighed. En liste bevarer rækkefølgen fra dashboardet.
type KV struct{ Key, Value string }

// App er en udgivet app og alt om den: ejer, deling, status, versioner, variabler og hemmeligheder.
// Slug er appens adresse (<slug>.<domæne>) og nøglen i DB.Apps.
type App struct {
	Slug, Name   string
	OwnerID      string
	Visibility   string   // private (standard), shared, members, public
	Access       []string // bruger-id'er, når Visibility er shared
	Port         int      // port appen lytter på inde i containeren
	HostPort     int      // port på værten (kun 127.0.0.1), som trafikken sendes til lige nu
	Slot         string   // aktiv container-plads: "" (vd-<slug>) eller "b" (vd-<slug>_b); skifter ved hver udrulning
	Status       string
	StatusText   string
	Approved     bool
	Current      int // version der kører (0 = ingen)
	Pending      int // nyeste version der venter på godkendelse
	NeedsRestart bool
	Versions     []Version
	Env          []KV
	Secrets      []KV       // tom Value = mangler
	Git          *GitSource // sat, når appen hentes fra git
	FromImage    string     // sat, når appen er et færdigt image (deploy_image)
	Created      time.Time
	Updated      time.Time
}

// GitSource husker, hvilket repo og hvilken gren en app hentes fra.
type GitSource struct{ Repo, Branch string }

// GitKey er en deploy key, som kun må bruges af sin ejer og til ét bestemt repo.
type GitKey struct {
	OwnerID, Repo, Public string
	Created               time.Time
}

// DB er hele serverens tilstand. Den gemmes samlet som JSON i state.json.
type DB struct {
	Users    []*User
	Sessions map[string]*Session
	Clients  map[string]*OAuthClient
	Codes    map[string]*AuthCode
	Tokens   map[string]*Token
	Apps     map[string]*App
	Invites  map[string]*Invite
	Tickets  map[string]*Ticket
	Uploads  map[string]*Upload
	GitKeys  map[string]*GitKey
	Settings Settings
}

// schemaVersion er versionen af databasens indhold. Den hæves, når en ny version ændrer data på en måde,
// som en ældre version ikke kan læse rigtigt. En ældre binær nægter at starte på nyere data, så en
// opdatering, der rulles tilbage, også lægger den tilsvarende backup af state.db tilbage (se update.go).
const schemaVersion = 1

// Settings er serverens egne indstillinger (én række, «server», i tabellen settings).
type Settings struct {
	Schema      int       // skemaversion, data sidst blev skrevet med
	AutoUpdate  bool      // installér nye versioner af Vibe Deploy automatisk på det valgte tidspunkt
	UpdateDay   string    // «daily» eller en ugedag («sun» …); tom = hver dag (se schedule.go)
	UpdateAt    string    // klokkeslæt i UpdateTZ, «HH:MM»; tom = 03:00
	UpdateTZ    string    // IANA-tidszone, fx «Europe/Copenhagen»; tom = Europe/Copenhagen
	LastAutoTry time.Time // seneste automatiske forsøg, så hvert tidspunkt kun prøves én gang
}

// Store holder hele tilstanden i hukommelsen og gemmer den i SQLite (state.db).
// Hver bruger, app, session osv. er én række med sine data som JSON; efter hver ændring skrives
// kun de rækker, der er ændret, i én transaktion. Hemmeligheder krypteres med box, før de gemmes.
type Store struct {
	mu    sync.Mutex
	sql   *sql.DB
	box   *secretBox
	db    DB
	saved map[string]map[string]string // tabel → id → JSON, som rækken sidst blev gemt (ukrypteret)
}

// tables er tabellerne i state.db. Alle har samme form: id, pos (rækkefølge) og data (JSON).
var tables = []string{"users", "sessions", "clients", "codes", "tokens", "apps", "invites", "tickets", "uploads", "gitkeys", "settings"}

// OpenStore åbner (eller opretter) dir/state.db. Findes der en state.json fra en tidligere version,
// og er databasen tom, flyttes dataene over, og filen omdøbes til state.json.importeret.
func OpenStore(dir string, box *secretBox) (*Store, error) {
	path := filepath.Join(dir, "state.db")
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close() // opret filen med 0600, før SQLite gør det med 0644
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // Store skriver alligevel én ad gangen
	for _, t := range tables {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + t + ` (id TEXT PRIMARY KEY, pos INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL)`); err != nil {
			db.Close()
			return nil, fmt.Errorf("state.db: %w", err)
		}
	}
	s := &Store{sql: db, box: box, saved: map[string]map[string]string{}}
	if err := s.load(); err != nil {
		db.Close()
		return nil, err
	}
	if s.db.Settings.Schema > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("state.db er skrevet af en nyere version af Vibe Deploy (skema %d; denne version kender skema %d). Brug den nyere version, eller læg backuppen fra før opdateringen tilbage", s.db.Settings.Schema, schemaVersion)
	}
	legacy := filepath.Join(dir, "state.json")
	imported := false
	if len(s.saved["users"]) == 0 && len(s.saved["apps"]) == 0 {
		if b, err := os.ReadFile(legacy); err == nil {
			if err := json.Unmarshal(b, &s.db); err != nil {
				db.Close()
				return nil, fmt.Errorf("state.json: %w", err)
			}
			imported = true
		}
	}
	s.db.init()
	s.db.migrate()
	s.db.Settings.Schema = schemaVersion
	if err := s.save(); err != nil {
		db.Close()
		return nil, err
	}
	if imported {
		if err := os.Rename(legacy, legacy+".importeret"); err == nil {
			fmt.Printf("state.json er flyttet til state.db, og hemmelighederne er krypteret. Den gamle fil hedder nu state.json.importeret\n" +
				"og indeholder hemmeligheder i klartekst: slet den, når du har tjekket, at alt virker.\n")
		}
	}
	return s, nil
}

// load læser alle tabeller ind i s.db og dekrypterer hemmelighederne.
func (s *Store) load() error {
	s.db = DB{}
	s.db.init()
	for _, t := range tables {
		s.saved[t] = map[string]string{}
		rows, err := s.sql.Query(`SELECT id, data FROM ` + t + ` ORDER BY pos, id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, data string
			if err := rows.Scan(&id, &data); err != nil {
				rows.Close()
				return err
			}
			if err := s.db.put(t, id, []byte(data)); err != nil {
				rows.Close()
				return fmt.Errorf("state.db %s/%s: %w", t, id, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for slug, a := range s.db.Apps {
		for i, kv := range a.Secrets {
			v, err := s.box.open(kv.Value, secretAD(slug, kv.Key))
			if err != nil {
				return fmt.Errorf("hemmeligheden %s i appen %s: %w. Starter serveren med en anden nøgle end sidst?", kv.Key, slug, err)
			}
			a.Secrets[i].Value = v
		}
	}
	// Det, der nu står i hukommelsen, svarer til databasen.
	for t, rs := range s.db.rows() {
		for id, r := range rs {
			s.saved[t][id] = r.json
		}
	}
	return nil
}

// secretAD er den ekstra data, en hemmelighed krypteres med: appen og navnet.
func secretAD(slug, key string) string { return "secret:" + slug + ":" + key }

// row er én række, som den gemmes: rækkefølge og JSON.
type row struct {
	pos  int
	json string
}

// rows laver hele tilstanden om til rækker pr. tabel (ukrypteret JSON).
func (d *DB) rows() map[string]map[string]row {
	out := map[string]map[string]row{}
	for _, t := range tables {
		out[t] = map[string]row{}
	}
	for i, u := range d.Users {
		b, _ := json.Marshal(u)
		out["users"][u.ID] = row{i, string(b)}
	}
	rowsOf(out["sessions"], d.Sessions)
	rowsOf(out["clients"], d.Clients)
	rowsOf(out["codes"], d.Codes)
	rowsOf(out["tokens"], d.Tokens)
	rowsOf(out["apps"], d.Apps)
	rowsOf(out["invites"], d.Invites)
	rowsOf(out["tickets"], d.Tickets)
	rowsOf(out["uploads"], d.Uploads)
	rowsOf(out["gitkeys"], d.GitKeys)
	b, _ := json.Marshal(d.Settings)
	out["settings"]["server"] = row{0, string(b)}
	return out
}

// rowsOf laver hvert element i m om til en række.
func rowsOf[T any](out map[string]row, m map[string]*T) {
	for k, v := range m {
		b, _ := json.Marshal(v)
		out[k] = row{0, string(b)}
	}
}

// put lægger én række fra databasen ind i tilstanden.
func (d *DB) put(t, id string, data []byte) error {
	switch t {
	case "users":
		u := &User{}
		d.Users = append(d.Users, u)
		return json.Unmarshal(data, u)
	case "sessions":
		return putIn(d.Sessions, id, data)
	case "clients":
		return putIn(d.Clients, id, data)
	case "codes":
		return putIn(d.Codes, id, data)
	case "tokens":
		return putIn(d.Tokens, id, data)
	case "apps":
		return putIn(d.Apps, id, data)
	case "invites":
		return putIn(d.Invites, id, data)
	case "tickets":
		return putIn(d.Tickets, id, data)
	case "uploads":
		return putIn(d.Uploads, id, data)
	case "gitkeys":
		return putIn(d.GitKeys, id, data)
	case "settings":
		return json.Unmarshal(data, &d.Settings)
	}
	return fmt.Errorf("ukendt tabel %s", t)
}

// putIn læser data ind i et nyt element i m under id.
func putIn[T any](m map[string]*T, id string, data []byte) error {
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	m[id] = v
	return nil
}

// init sørger for, at alle maps findes, så koden kan skrive i dem uden nil-tjek.
func (d *DB) init() {
	if d.Sessions == nil {
		d.Sessions = map[string]*Session{}
	}
	if d.Clients == nil {
		d.Clients = map[string]*OAuthClient{}
	}
	if d.Codes == nil {
		d.Codes = map[string]*AuthCode{}
	}
	if d.Tokens == nil {
		d.Tokens = map[string]*Token{}
	}
	if d.Apps == nil {
		d.Apps = map[string]*App{}
	}
	if d.Invites == nil {
		d.Invites = map[string]*Invite{}
	}
	if d.Tickets == nil {
		d.Tickets = map[string]*Ticket{}
	}
	if d.Uploads == nil {
		d.Uploads = map[string]*Upload{}
	}
	if d.GitKeys == nil {
		d.GitKeys = map[string]*GitKey{}
	}
}

// user slår en bruger op på id; nil, hvis brugeren ikke findes.
func (d *DB) user(id string) *User {
	for _, u := range d.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

// migrate opdaterer data fra tidligere versioner: den første bruger bliver ejer,
// og eksisterende apps bliver private (sikreste standard).
func (d *DB) migrate() {
	if len(d.Users) == 0 {
		return
	}
	var owner *User
	for _, u := range d.Users {
		if u.Role == RoleOwner {
			owner = u
		}
	}
	if owner == nil {
		owner = d.Users[0]
		owner.Role, owner.Builder = RoleOwner, true
	}
	for _, u := range d.Users {
		if u.Role == "" {
			u.Role = RoleMember
		}
	}
	for _, a := range d.Apps {
		if a.OwnerID == "" {
			a.OwnerID = owner.ID
		}
		if a.Visibility == "" {
			a.Visibility = VisPrivate
		}
	}
}

// View kører f under lås uden at kopiere hele tilstanden (til hyppige opslag).
func (s *Store) View(f func(db *DB)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.db)
}

// Do kører f under lås og gemmer bagefter, hvis f ikke fejler.
func (s *Store) Do(f func(db *DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := f(&s.db); err != nil {
		return err
	}
	return s.save()
}

// Snapshot returnerer en dyb kopi, som kan læses uden lås.
func (s *Store) Snapshot() DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(&s.db)
	var c DB
	_ = json.Unmarshal(b, &c)
	c.init()
	return c
}

// save skriver de rækker, der er ændret siden sidst, og sletter dem, der er væk, i én transaktion.
// Apps gemmes med krypterede hemmeligheder. Kaldes med s.mu låst.
func (s *Store) save() error {
	cur := s.db.rows()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range tables {
		for id, r := range cur[t] {
			if old, ok := s.saved[t][id]; ok && old == r.json {
				continue
			}
			data := r.json
			if t == "apps" {
				data = s.sealedApp(id)
			}
			if _, err := tx.Exec(`INSERT INTO `+t+` (id, pos, data) VALUES (?, ?, ?) ON CONFLICT(id) DO UPDATE SET pos = excluded.pos, data = excluded.data`, id, r.pos, data); err != nil {
				return err
			}
		}
		for id := range s.saved[t] {
			if _, ok := cur[t][id]; !ok {
				if _, err := tx.Exec(`DELETE FROM `+t+` WHERE id = ?`, id); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, t := range tables {
		s.saved[t] = map[string]string{}
		for id, r := range cur[t] {
			s.saved[t][id] = r.json
		}
	}
	return nil
}

// sealedApp er appens JSON med krypterede hemmeligheder, som den skrives i databasen.
func (s *Store) sealedApp(slug string) string {
	cp := *s.db.Apps[slug]
	cp.Secrets = make([]KV, len(cp.Secrets))
	for i, kv := range s.db.Apps[slug].Secrets {
		cp.Secrets[i] = kv
		if kv.Value != "" {
			cp.Secrets[i].Value = s.box.seal(kv.Value, secretAD(slug, kv.Key))
		}
	}
	b, _ := json.Marshal(&cp)
	return string(b)
}

// BackupTo skriver en sammenhængende kopi af databasen til path (VACUUM INTO), også mens serveren kører.
// Hemmelighederne forbliver krypterede i kopien.
func (s *Store) BackupTo(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(path)
	if _, err := s.sql.Exec(`VACUUM INTO ?`, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Close lukker databasen.
func (s *Store) Close() error { return s.sql.Close() }
