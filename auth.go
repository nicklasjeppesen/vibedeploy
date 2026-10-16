package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// randomToken returnerer n kryptografisk tilfældige bytes som hex (2n tegn).
// Bruges til id'er, sessioner, billetter og adgangstokens.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// PBKDF2-HMAC-SHA256 (RFC 8018), skrevet ud, så prototypen ikke har afhængigheder.
func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	var dk []byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// hashPassword udleder adgangskodens hash med PBKDF2 (210.000 runder) og det hex-kodede salt.
func hashPassword(pw, saltHex string) string {
	salt, _ := hex.DecodeString(saltHex)
	return hex.EncodeToString(pbkdf2([]byte(pw), salt, 210_000, 32))
}

// checkPassword sammenligner i konstant tid, så svartiden ikke afslører noget om hashen.
func checkPassword(u *User, pw string) bool {
	return subtle.ConstantTimeCompare([]byte(hashPassword(pw, u.Salt)), []byte(u.Hash)) == 1
}

const cookieName = "vd_session"

// startSession opretter en dashboard-session (14 dage) og sætter session-cookien.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	tok := randomToken(32)
	err := s.st.Do(func(db *DB) error {
		db.Sessions[tok] = &Session{UserID: userID, Expires: time.Now().Add(14 * 24 * time.Hour)}
		return nil
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.baseURL(r), "https://"), MaxAge: 14 * 24 * 3600,
	})
	return nil
}

// currentUser finder den loggede bruger ud fra session-cookien, eller nil.
// App-sessioner (AppSlug sat) gælder kun på en apps adresse, ikke i dashboardet.
func (s *Server) currentUser(r *http.Request) *User {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	db := s.st.Snapshot()
	sess := db.Sessions[c.Value]
	if sess == nil || sess.AppSlug != "" || time.Now().After(sess.Expires) {
		return nil
	}
	for _, u := range db.Users {
		if u.ID == sess.UserID {
			return u
		}
	}
	return nil
}

// requireUser sender ikke-loggede brugere til login (eller opret konto, hvis der ingen er).
func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := s.currentUser(r); u != nil {
			if r.Method == http.MethodPost && !sameOrigin(r) {
				http.Error(w, "Forbudt (forkert afsender)", http.StatusForbidden)
				return
			}
			h(w, r, u)
			return
		}
		db := s.st.Snapshot()
		if len(db.Users) == 0 {
			http.Redirect(w, r, "/signup", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	}
}

// sameOrigin er en enkel CSRF-beskyttelse oven på SameSite=Lax.
func sameOrigin(r *http.Request) bool {
	for _, h := range []string{"Origin", "Referer"} {
		if v := r.Header.Get(h); v != "" {
			u, err := url.Parse(v)
			return err == nil && u.Host == r.Host
		}
	}
	return true
}

// safeNext tillader kun interne stier, så login ikke kan bruges til omdirigering til andre sider.
func safeNext(n string) string {
	if strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") && !strings.Contains(n, "\\") {
		return n
	}
	return "/"
}
