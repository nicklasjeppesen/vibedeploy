package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testBox er en secretBox med en fast nøgle til tests.
func testBox(t *testing.T) *secretBox {
	t.Helper()
	b, err := newSecretBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawApp læser appens række direkte fra state.db, som den ligger på disken.
func rawApp(t *testing.T, dir, slug string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var data string
	if err := db.QueryRow(`SELECT data FROM apps WHERE id = ?`, slug).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStoreRoundTripAndEncryption(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	err = st.Do(func(db *DB) error {
		db.Users = append(db.Users, &User{ID: "u1", Name: "Anna", Role: RoleOwner}, &User{ID: "u2", Name: "Bo"})
		db.Apps["shop"] = &App{Slug: "shop", OwnerID: "u1", Secrets: []KV{{"STRIPE_KEY", "sk_live_hemmelig"}, {"TOM", ""}}}
		db.Sessions["s1"] = &Session{UserID: "u1"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw := rawApp(t, dir, "shop"); strings.Contains(raw, "sk_live_hemmelig") || !strings.Contains(raw, encPrefix) {
		t.Fatalf("hemmeligheden skal være krypteret på disken: %s", raw)
	}
	st.Close()

	st, err = OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	db := st.Snapshot()
	if len(db.Users) != 2 || db.Users[0].ID != "u1" || db.Users[1].ID != "u2" {
		t.Fatalf("brugere og rækkefølge skal bevares: %+v", db.Users)
	}
	if got := db.Apps["shop"].Secrets; got[0].Value != "sk_live_hemmelig" || got[1].Value != "" {
		t.Fatalf("hemmeligheder blev ikke læst rigtigt: %+v", got)
	}

	// Slettede rækker skal også forsvinde fra databasen.
	_ = st.Do(func(db *DB) error { delete(db.Sessions, "s1"); return nil })
	st.Close()
	st, _ = OpenStore(dir, testBox(t))
	if len(st.Snapshot().Sessions) != 0 {
		t.Fatal("sessionen burde være slettet")
	}
	st.Close()
}

func TestStoreWrongKey(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(dir, testBox(t))
	_ = st.Do(func(db *DB) error {
		db.Users = append(db.Users, &User{ID: "u1", Role: RoleOwner})
		db.Apps["a"] = &App{Slug: "a", OwnerID: "u1", Secrets: []KV{{"K", "v"}}}
		return nil
	})
	st.Close()
	other, _ := newSecretBox(bytes.Repeat([]byte{8}, 32))
	if _, err := OpenStore(dir, other); err == nil {
		t.Fatal("en forkert nøgle skal give en fejl i stedet for tomme hemmeligheder")
	}
}

func TestStoreImportsLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	old := DB{Users: []*User{{ID: "u1", Name: "Anna"}}, Apps: map[string]*App{"blog": {Slug: "blog", Secrets: []KV{{"DB_PASSWORD", "klartekst"}}}}}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(dir, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	db := st.Snapshot()
	if db.Users[0].Role != RoleOwner || db.Apps["blog"].OwnerID != "u1" || db.Apps["blog"].Visibility != VisPrivate {
		t.Fatal("gamle data skal migreres som før (første bruger ejer, apps private)")
	}
	if db.Apps["blog"].Secrets[0].Value != "klartekst" {
		t.Fatal("hemmeligheden skal med over")
	}
	if strings.Contains(rawApp(t, dir, "blog"), "klartekst") {
		t.Fatal("den importerede hemmelighed skal krypteres")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.importeret")); err != nil {
		t.Fatal("state.json skal omdøbes efter import")
	}
	if err := st.BackupTo(filepath.Join(dir, "kopi.db")); err != nil {
		t.Fatal(err)
	}
	st.Close()
}

func TestSecretBoxBindsToName(t *testing.T) {
	b := testBox(t)
	c := b.seal("v", secretAD("a", "K"))
	if v, err := b.open(c, secretAD("a", "K")); err != nil || v != "v" {
		t.Fatal("kunne ikke åbne egen værdi")
	}
	if _, err := b.open(c, secretAD("b", "K")); err == nil {
		t.Fatal("en værdi må ikke kunne flyttes til en anden app")
	}
	if v, _ := b.open("gammel", "x"); v != "gammel" {
		t.Fatal("ukrypterede værdier fra før skal kunne læses")
	}
}
