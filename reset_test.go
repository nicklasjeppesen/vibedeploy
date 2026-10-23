package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newResetServer laver en server med en ejer og et medlem, som begge har en adgangskode og en session.
func newResetServer(t *testing.T) (*Server, *User, *User) {
	t.Helper()
	m := newTestManager(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	s := &Server{st: m.st, mgr: m, pages: loadPages(), domain: "min-server.dk", publicURL: "https://min-server.dk", resetKey: resetMACKey(key)}
	owner := &User{ID: "o", Email: "nicklas@live.dk", Name: "Nicklas", Role: RoleOwner, Salt: "00", Hash: hashPassword("gammel-kode-123", "00")}
	member := &User{ID: "m", Email: "Mette@Example.dk", Name: "Mette", Role: RoleMember, Salt: "00", Hash: hashPassword("gammel-kode-123", "00")}
	_ = s.st.Do(func(db *DB) error {
		db.Users = append(db.Users, owner, member)
		db.Sessions["s-o"] = &Session{UserID: "o", Expires: time.Now().Add(time.Hour)}
		db.Sessions["s-m"] = &Session{UserID: "m", Expires: time.Now().Add(time.Hour)}
		db.Sessions["s-m-app"] = &Session{UserID: "m", AppSlug: "blog", Expires: time.Now().Add(time.Hour)}
		db.Tokens["t-m"] = &Token{UserID: "m", Kind: "pat", Expires: time.Now().Add(time.Hour)}
		return nil
	})
	return s, owner, member
}

// submitReset sender formularen på /reset/<tok>.
func submitReset(s *Server, tok, pw, pw2 string) *httptest.ResponseRecorder {
	form := url.Values{"password": {pw}, "password2": {pw2}}
	r := httptest.NewRequest("POST", "https://min-server.dk/reset/"+tok, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

// linkToken er tokenet i et link som https://min-server.dk/reset/<token>.
func linkToken(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "/reset/")
	if i < 0 {
		t.Fatalf("intet link i svaret:\n%s", body)
	}
	tok := body[i+len("/reset/"):]
	if j := strings.IndexAny(tok, "<\" \n"); j >= 0 {
		tok = tok[:j]
	}
	return tok
}

func TestResetFromPeoplePage(t *testing.T) {
	s, owner, member := newResetServer(t)

	// Ejeren laver et link til Mette; kun hashet gemmes.
	r := httptest.NewRequest("POST", "https://min-server.dk/people/user/m/reset", nil)
	r.SetPathValue("id", "m")
	w := httptest.NewRecorder()
	s.peopleReset(w, r, owner)
	tok := linkToken(t, w.Body.String())
	db := s.st.Snapshot()
	if u := db.user("m"); u.ResetHash != resetHash(tok) || strings.Contains(u.ResetHash, tok) {
		t.Fatal("kun et hash af linket må gemmes")
	}

	// Siden viser formularen; forkerte input forklares og bruger ikke linket op.
	pr := httptest.NewRequest("GET", "https://min-server.dk/reset/"+tok, nil)
	pw := httptest.NewRecorder()
	s.handler().ServeHTTP(pw, pr)
	if !strings.Contains(pw.Body.String(), "Mette") || !strings.Contains(pw.Body.String(), `name="password2"`) {
		t.Fatalf("formularen vises ikke:\n%s", pw.Body.String())
	}
	if w := submitReset(s, tok, "kort", "kort"); !strings.Contains(w.Body.String(), "mindst 12 tegn") {
		t.Fatal("for kort adgangskode skal forklares")
	}
	if w := submitReset(s, tok, "ny-kode-123456", "ny-kode-654321"); !strings.Contains(w.Body.String(), "ikke ens") {
		t.Fatal("to forskellige adgangskoder skal forklares")
	}

	// Rigtig adgangskode: gemmes, linket bruges op, Mette logges ud alle steder og ind her. AI-adgangen består.
	w = submitReset(s, tok, "ny-kode-123456", "ny-kode-123456")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?m=pwreset" || len(w.Result().Cookies()) == 0 {
		t.Fatalf("efter ny adgangskode: %d %s", w.Code, w.Header().Get("Location"))
	}
	db = s.st.Snapshot()
	u := db.user("m")
	if !checkPassword(u, "ny-kode-123456") || checkPassword(u, "gammel-kode-123") || u.ResetHash != "" {
		t.Fatal("den nye adgangskode skal virke, den gamle ikke, og linket skal være brugt")
	}
	if db.Sessions["s-m"] != nil || db.Sessions["s-m-app"] != nil || db.Sessions["s-o"] == nil {
		t.Fatal("kun Mettes gamle sessioner må slettes")
	}
	if db.Tokens["t-m"] == nil {
		t.Fatal("AI-adgangen skal bestå")
	}
	if w := submitReset(s, tok, "tredje-kode-123", "tredje-kode-123"); !strings.Contains(w.Body.String(), "brugt eller udløbet") {
		t.Fatal("et brugt link må ikke virke igen")
	}
	_ = member

	// Ejeren kan ikke lave et link til sig selv på siden (han er jo logget ind).
	r = httptest.NewRequest("POST", "https://min-server.dk/people/user/o/reset", nil)
	r.SetPathValue("id", "o")
	w = httptest.NewRecorder()
	s.peopleReset(w, r, owner)
	if snapUser(s, "o").ResetHash != "" {
		t.Fatal("ejeren må ikke lave et link til sig selv på siden Personer")
	}
}

func TestResetLinkExpiresAndIsReplaced(t *testing.T) {
	s, _, _ := newResetServer(t)
	_ = s.st.Do(func(db *DB) error {
		setReset(db.user("m"), resetHash("gammelt"), time.Now())
		setReset(db.user("m"), resetHash("nyt"), time.Now()) // et nyt link erstatter det gamle
		return nil
	})
	if w := submitReset(s, "gammelt", "ny-kode-123456", "ny-kode-123456"); !strings.Contains(w.Body.String(), "brugt eller udløbet") {
		t.Fatal("det gamle link skal være ugyldigt, når der er lavet et nyt")
	}
	_ = s.st.Do(func(db *DB) error { db.user("m").ResetExpires = time.Now().Add(-time.Second); return nil })
	if w := submitReset(s, "nyt", "ny-kode-123456", "ny-kode-123456"); !strings.Contains(w.Body.String(), "brugt eller udløbet") {
		t.Fatal("et udløbet link må ikke virke")
	}
	if (&DB{Users: s.st.Snapshot().Users}).userByReset("", time.Now()) != nil {
		t.Fatal("et tomt token må aldrig passe")
	}
}

// TestResetPasswordCommand kører kommandoens HTTP-del mod en rigtig server på 127.0.0.1, så også reglen
// for besøg på IP-adressen er med (Caddy-fejlen med tls-ask må ikke gentage sig her).
func TestResetPasswordCommand(t *testing.T) {
	s, _, _ := newResetServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	link, name, err := requestReset(addr, key, "NICKLAS@live.dk")
	if err != nil || name != "Nicklas" || !strings.HasPrefix(link, "https://min-server.dk/reset/") {
		t.Fatalf("kommandoen: %q %q %v", link, name, err)
	}
	if w := submitReset(s, strings.TrimPrefix(link, "https://min-server.dk/reset/"), "ny-ejer-kode-1", "ny-ejer-kode-1"); w.Code != http.StatusSeeOther {
		t.Fatalf("linket fra kommandoen skal virke: %d", w.Code)
	}
	if !checkPassword(snapUser(s, "o"), "ny-ejer-kode-1") {
		t.Fatal("ejerens nye adgangskode er ikke gemt")
	}

	if _, _, err := requestReset(addr, key, "findes@ikke.dk"); err == nil || !strings.Contains(err.Error(), "ingen konto") {
		t.Fatalf("ukendt e-mail skal forklares, fik %v", err)
	}
	wrong := make([]byte, 32)
	if _, _, err := requestReset(addr, wrong, "nicklas@live.dk"); err == nil || !strings.Contains(err.Error(), "signeret") {
		t.Fatalf("en forkert nøgle skal afvises, fik %v", err)
	}
}

func TestInternalResetGuards(t *testing.T) {
	s, _, _ := newResetServer(t)
	send := func(remote, fwd, body, mac string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/internal/reset-password", strings.NewReader(body))
		r.RemoteAddr = remote
		if fwd != "" {
			r.Header.Set("X-Forwarded-For", fwd)
		}
		r.Header.Set("X-Vibedeploy-Mac", mac)
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w.Code
	}
	h := resetHash("x")
	fresh := `{"email":"nicklas@live.dk","hash":"` + h + `","expires":` + strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10) + `}`
	old := `{"email":"nicklas@live.dk","hash":"` + h + `","expires":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) + `}`
	far := `{"email":"nicklas@live.dk","hash":"` + h + `","expires":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`
	mac := func(b string) string { return resetMAC(s.resetKey, []byte(b)) }

	if c := send("10.0.0.5:1", "", fresh, mac(fresh)); c != http.StatusNotFound {
		t.Errorf("udefra: %d, ventede 404", c)
	}
	if c := send("127.0.0.1:1", "1.2.3.4", fresh, mac(fresh)); c != http.StatusNotFound {
		t.Errorf("via Caddy (X-Forwarded-For): %d, ventede 404", c)
	}
	if c := send("127.0.0.1:1", "", fresh, "forkert"); c != http.StatusForbidden {
		t.Errorf("uden rigtig signatur: %d, ventede 403", c)
	}
	if c := send("127.0.0.1:1", "", old, mac(old)); c != http.StatusBadRequest {
		t.Errorf("udløbet forespørgsel: %d, ventede 400", c)
	}
	if c := send("127.0.0.1:1", "", far, mac(far)); c != http.StatusBadRequest {
		t.Errorf("forespørgsel, der gælder for længe: %d, ventede 400", c)
	}
	if snapUser(s, "o").ResetHash != "" {
		t.Fatal("ingen af de afviste forespørgsler må lave et link")
	}
	if c := send("127.0.0.1:1", "", fresh, mac(fresh)); c != http.StatusOK {
		t.Errorf("gyldig forespørgsel: %d, ventede 200", c)
	}
	if printable("ok\x1b[2Jx\u0085") != "ok[2Jx" {
		t.Fatalf("styretegn skal fjernes: %q", printable("ok\x1b[2Jx\u0085"))
	}
}

// snapUser er kontoen med id i en kopi af tilstanden.
func snapUser(s *Server, id string) *User {
	db := s.st.Snapshot()
	return db.user(id)
}
