package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------- Nulstilling af adgangskode ----------
//
// Der sendes ingen mails. I stedet laves et engangslink (/reset/<token>), som virker én gang i resetTTL:
//   - Ejeren laver et link til en anden konto på siden Personer og sender det selv videre.
//   - Har ejeren selv glemt sin adgangskode, kører han «sudo vibedeploy reset-password <e-mail>» på serveren.
//     Kommandoen læser serverens nøgle (/etc/vibedeploy/secret.key), som kun root og tjenesten har, og beder
//     den kørende server om at gemme linket. Den, der kan det, ejer i praksis serveren.
// Kun et hash af linket gemmes (User.ResetHash), så det ikke kan læses ud af databasen. Et nyt link
// erstatter det forrige. Når adgangskoden er skiftet, logges kontoen ud alle steder; AI-adgangene består.

// resetTTL er, hvor længe et link til ny adgangskode virker.
const resetTTL = time.Hour

// resetHash er det hash af et engangslink, der gemmes i databasen.
func resetHash(token string) string {
	sum := sha256.Sum256([]byte("vibedeploy-reset-v1\n" + token))
	return hex.EncodeToString(sum[:])
}

// resetMACKey er nøglen, kommandoen reset-password signerer sin forespørgsel med. Den udledes af
// serverens nøgle til hemmeligheder, så den aldrig bruges direkte til to ting.
func resetMACKey(secretKey []byte) []byte {
	m := hmac.New(sha256.New, secretKey)
	m.Write([]byte("vibedeploy-reset-password-v1"))
	return m.Sum(nil)
}

// resetMAC er signaturen (hex) for en forespørgsel fra kommandoen reset-password.
func resetMAC(key, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// setReset gemmer et nyt engangslink for kontoen u og erstatter et tidligere. Kaldes inde i st.Do.
func setReset(u *User, hash string, now time.Time) {
	u.ResetHash, u.ResetExpires = hash, now.Add(resetTTL)
}

// userByEmail finder en konto ud fra e-mail (uden forskel på store og små bogstaver).
func (d *DB) userByEmail(email string) *User {
	for _, u := range d.Users {
		if strings.EqualFold(u.Email, strings.TrimSpace(email)) {
			return u
		}
	}
	return nil
}

// userByReset finder kontoen, som et gyldigt (ikke udløbet) engangslink hører til.
func (d *DB) userByReset(token string, now time.Time) *User {
	if token == "" {
		return nil
	}
	h := resetHash(token)
	for _, u := range d.Users {
		if u.ResetHash != "" && hmac.Equal([]byte(u.ResetHash), []byte(h)) && now.Before(u.ResetExpires) {
			return u
		}
	}
	return nil
}

// ---------- Siden Personer: ejeren laver et link til en anden konto ----------

// peopleReset laver et engangslink til ny adgangskode for en anden konto og viser det på siden Personer.
func (s *Server) peopleReset(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("id")
	tok := randomToken(24)
	var name string
	_ = s.st.Do(func(db *DB) error {
		if t := db.user(id); t != nil && t.ID != u.ID {
			setReset(t, resetHash(tok), time.Now())
			name = t.Name
		}
		return nil
	})
	if name == "" {
		s.page(w, r, "message.html", "Det lykkedes ikke", map[string]any{"Text": "Den konto findes ikke. Din egen adgangskode skifter du ved at logge ud og bruge kommandoen reset-password på serveren (se README).", "Back": "/people"})
		return
	}
	s.renderPeopleWith(w, r, map[string]any{"ResetLink": s.baseURL(r) + "/reset/" + tok, "ResetName": name})
}

// ---------- Selve linket: vælg en ny adgangskode ----------

// resetPage viser formularen til en ny adgangskode, hvis linket er gyldigt.
func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	var name string
	s.st.View(func(db *DB) {
		if u := db.userByReset(r.PathValue("token"), time.Now()); u != nil {
			name = u.Name
		}
	})
	if name == "" {
		s.resetInvalid(w, r)
		return
	}
	s.page(w, r, "reset.html", "Ny adgangskode", map[string]any{"Name": name})
}

// resetInvalid forklarer, at linket er brugt eller udløbet, og hvad man så gør.
func (s *Server) resetInvalid(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "message.html", "Linket virker ikke", map[string]any{
		"Text": "Linket til ny adgangskode er brugt eller udløbet (det virker én gang i en time).\n\nBed ejeren af serveren om et nyt. Er du selv ejeren, så kør denne kommando på serveren:\n\nsudo vibedeploy reset-password din@email.dk",
		"Back": "/login"})
}

// resetSubmit gemmer den nye adgangskode, bruger linket op, logger kontoen ud alle steder og logger ind her.
func (s *Server) resetSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Forbudt", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	tok := r.PathValue("token")
	pw, pw2 := r.PostForm.Get("password"), r.PostForm.Get("password2")
	var name string
	s.st.View(func(db *DB) {
		if u := db.userByReset(tok, time.Now()); u != nil {
			name = u.Name
		}
	})
	if name == "" {
		s.resetInvalid(w, r)
		return
	}
	fail := func(msg string) {
		s.page(w, r, "reset.html", "Ny adgangskode", map[string]any{"Name": name, "Err": msg})
	}
	if len([]rune(pw)) < 12 {
		fail("Adgangskoden skal være mindst 12 tegn.")
		return
	}
	if pw != pw2 {
		fail("De to adgangskoder er ikke ens. Skriv den samme to gange.")
		return
	}
	salt := randomToken(16)
	hash := hashPassword(pw, salt) // uden for låsen: det tager lidt tid
	var uid string
	_ = s.st.Do(func(db *DB) error {
		u := db.userByReset(tok, time.Now())
		if u == nil {
			return nil
		}
		u.Salt, u.Hash, u.ResetHash, u.ResetExpires = salt, hash, "", time.Time{}
		uid = u.ID
		for k, sess := range db.Sessions { // log ud alle steder, også på appenes adresser
			if sess.UserID == u.ID {
				delete(db.Sessions, k)
			}
		}
		return nil
	})
	if uid == "" {
		s.resetInvalid(w, r)
		return
	}
	if err := s.startSession(w, r, uid); err != nil {
		http.Error(w, "Kunne ikke starte session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?m=pwreset", http.StatusSeeOther)
}

// ---------- vibedeploy reset-password (kommandoen på serveren) ----------

// resetRequest er det, kommandoen sender til den kørende server. Linket selv sendes ikke; kun hashet.
type resetRequest struct {
	Email   string `json:"email"`
	Hash    string `json:"hash"`
	Expires int64  `json:"expires"` // unix-tid; forespørgslen må ikke genbruges senere
}

// resetResponse er serverens svar til kommandoen.
type resetResponse struct {
	Name  string `json:"name,omitempty"`
	Base  string `json:"base,omitempty"`
	Error string `json:"error,omitempty"`
}

// internalReset tager imod forespørgsler fra kommandoen reset-password. Den svarer kun på 127.0.0.1 og
// kun, hvis forespørgslen er signeret med en nøgle udledt af serverens nøgle til hemmeligheder.
func (s *Server) internalReset(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || r.Header.Get("X-Forwarded-For") != "" || len(s.resetKey) == 0 {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	reply := func(code int, res resetResponse) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(res)
	}
	if err != nil || !hmac.Equal([]byte(r.Header.Get("X-Vibedeploy-Mac")), []byte(resetMAC(s.resetKey, body))) {
		reply(http.StatusForbidden, resetResponse{Error: "forespørgslen er ikke signeret med serverens nøgle (brug -key-file, hvis nøglen ligger et andet sted end " + keyFilePath + ")"})
		return
	}
	var req resetRequest
	now := time.Now()
	if json.Unmarshal(body, &req) != nil || len(req.Hash) != 64 || now.Unix() > req.Expires || req.Expires > now.Add(5*time.Minute).Unix() {
		reply(http.StatusBadRequest, resetResponse{Error: "forespørgslen er udløbet eller ugyldig; prøv igen (passer serverens ur?)"})
		return
	}
	var name string
	_ = s.st.Do(func(db *DB) error {
		if u := db.userByEmail(req.Email); u != nil {
			setReset(u, req.Hash, now)
			name = u.Name
		}
		return nil
	})
	if name == "" {
		reply(http.StatusNotFound, resetResponse{Error: "der findes ingen konto med e-mailen " + req.Email})
		return
	}
	fmt.Printf("Link til ny adgangskode lavet for %s via kommandoen reset-password\n", req.Email)
	reply(http.StatusOK, resetResponse{Name: name, Base: s.dashBase()})
}

// printable fjerner tegn, der ikke kan skrives sikkert i en terminal (fx escape-koder).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// runResetPassword er «vibedeploy reset-password <e-mail>»: laver et engangslink til ny adgangskode.
func runResetPassword(args []string) {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	keyFile := fs.String("key-file", "", "serverens nøgle til hemmeligheder (standard: "+keyFilePath+", ellers ./data/secret.key)")
	addr := fs.String("addr", "127.0.0.1:8080", "adressen, Vibe Deploy lytter på")
	fs.Parse(args)
	if fs.NArg() != 1 || !strings.Contains(fs.Arg(0), "@") {
		fatalf("brug: sudo vibedeploy reset-password din@email.dk")
	}
	path := *keyFile
	if path == "" {
		path = keyFilePath
		if !fileExists(path) && fileExists("data/secret.key") {
			path = "data/secret.key" // når man kører lokalt med go run .
		}
	}
	key, err := loadKey(path, false)
	if err != nil {
		if os.IsPermission(err) {
			fatalf("kunne ikke læse %s. Kør kommandoen med sudo", path)
		}
		fatalf("kunne ikke læse serverens nøgle: %v", err)
	}
	link, name, err := requestReset(*addr, key, fs.Arg(0))
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("Link til ny adgangskode for %s:\n\n  %s\n\nÅbn det i din browser. Det virker én gang i en time. Del det ikke med andre.\n", name, link)
}

// requestReset beder den kørende server på addr om at gemme et nyt engangslink for email og returnerer
// linket og kontoens navn. Svaret er renset for tegn, der ikke kan skrives sikkert i en terminal.
func requestReset(addr string, key []byte, email string) (link, name string, err error) {
	tok := randomToken(24)
	body, _ := json.Marshal(resetRequest{Email: email, Hash: resetHash(tok), Expires: time.Now().Add(time.Minute).Unix()})
	req, _ := http.NewRequest("POST", "http://"+addr+"/internal/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vibedeploy-Mac", resetMAC(resetMACKey(key), body))
	// Følg aldrig en omdirigering: forespørgslen må kun gå til serveren på addr.
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("kunne ikke nå Vibe Deploy på %s. Kører den? (systemctl status vibedeploy): %v", addr, err)
	}
	defer resp.Body.Close()
	var res resetResponse
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&res)
	if resp.StatusCode != http.StatusOK {
		if res.Error == "" {
			res.Error = resp.Status + " (er Vibe Deploy for gammel til denne kommando? Opdatér den først)"
		}
		return "", "", fmt.Errorf("%s", printable(res.Error))
	}
	return printable(strings.TrimRight(res.Base, "/")) + "/reset/" + tok, printable(res.Name), nil
}
