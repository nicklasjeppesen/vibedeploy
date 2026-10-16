package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// writeJSON sender v som JSON med den givne statuskode.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// baseURL er serverens adresse, som klienten ser den: -public-url, hvis den er sat,
// ellers ud fra Host-headeren og X-Forwarded-Proto (sat af Caddy).
func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// protectedResourceMeta svarer på /.well-known/oauth-protected-resource (RFC 9728)
// og fortæller MCP-klienten, hvilken login-server der beskytter /mcp.
func (s *Server) protectedResourceMeta(w http.ResponseWriter, r *http.Request) {
	b := s.baseURL(r)
	writeJSON(w, 200, map[string]any{
		"resource":                 b + "/mcp",
		"authorization_servers":    []string{b},
		"bearer_methods_supported": []string{"header"},
	})
}

// authServerMeta svarer på /.well-known/oauth-authorization-server (RFC 8414)
// med adresserne til login, token og registrering. Kun authorization code + PKCE understøttes.
func (s *Server) authServerMeta(w http.ResponseWriter, r *http.Request) {
	b := s.baseURL(r)
	writeJSON(w, 200, map[string]any{
		"issuer":                                b,
		"authorization_endpoint":                b + "/authorize",
		"token_endpoint":                        b + "/token",
		"registration_endpoint":                 b + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// Dynamisk klientregistrering (RFC 7591), så AI-værktøjet kan melde sig selv ind.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		writeJSON(w, 400, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	for _, ru := range req.RedirectURIs {
		u, err := url.Parse(ru)
		if err != nil || u.Scheme == "" || u.Fragment != "" || map[string]bool{"javascript": true, "data": true, "file": true}[strings.ToLower(u.Scheme)] {
			writeJSON(w, 400, map[string]string{"error": "invalid_redirect_uri"})
			return
		}
	}
	id := "vdc_" + randomToken(12)
	err := s.st.Do(func(db *DB) error {
		if len(db.Clients) >= 200 {
			return http.ErrAbortHandler
		}
		name := req.ClientName
		if len(name) > 80 {
			name = name[:80]
		}
		db.Clients[id] = &OAuthClient{Name: name, RedirectURIs: req.RedirectURIs, Created: time.Now()}
		return nil
	})
	if err != nil {
		writeJSON(w, 429, map[string]string{"error": "too_many_clients"})
		return
	}
	writeJSON(w, 201, map[string]any{
		"client_id": id, "client_name": req.ClientName, "redirect_uris": req.RedirectURIs,
		"grant_types": []string{"authorization_code"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// authzParams er de /authorize-parametre, der skal med fra samtykkesiden til formularens svar.
type authzParams struct{ ClientID, RedirectURI, State, Challenge string }

// checkAuthz validerer /authorize-parametre mod den registrerede klient.
func (s *Server) checkAuthz(get func(string) string) (*OAuthClient, authzParams, string) {
	p := authzParams{get("client_id"), get("redirect_uri"), get("state"), get("code_challenge")}
	db := s.st.Snapshot()
	c := db.Clients[p.ClientID]
	if c == nil {
		return nil, p, "Ukendt AI-værktøj. Prøv at tilføje forbindelsen igen."
	}
	ok := false
	for _, ru := range c.RedirectURIs {
		if ru == p.RedirectURI {
			ok = true
		}
	}
	if !ok {
		return nil, p, "Returadressen passer ikke til det registrerede AI-værktøj."
	}
	if get("response_type") != "code" || p.Challenge == "" || get("code_challenge_method") != "S256" {
		return nil, p, "Forespørgslen mangler sikkerhedsparametre (PKCE)."
	}
	return c, p, ""
}

// authorizePage viser samtykkesiden («Giv din AI adgang») for en logget bruger.
// Kun brugere med bygge-ret kan forbinde en AI.
func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request, u *User) {
	if !canBuild(u) {
		s.page(w, r, "message.html", "Ingen adgang", map[string]any{"Text": "Din konto må ikke forbinde en AI. Bed ejeren om tilladelse til at bygge apps.", "Back": "/"})
		return
	}
	c, p, msg := s.checkAuthz(r.URL.Query().Get)
	if msg != "" {
		s.page(w, r, "message.html", "Kan ikke forbinde", map[string]any{"Text": msg})
		return
	}
	s.page(w, r, "consent.html", "Giv din AI adgang", map[string]any{"Client": c.Name, "P": p, "Query": r.URL.RawQuery})
}

// authorizeSubmit håndterer Tillad/Afvis. Ved Tillad laves en engangskode (10 minutter),
// og browseren sendes tilbage til AI-værktøjets returadresse med koden.
func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request, u *User) {
	if !canBuild(u) {
		http.Error(w, "Ingen adgang", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	c, p, msg := s.checkAuthz(r.PostForm.Get)
	if msg != "" || c == nil {
		s.page(w, r, "message.html", "Kan ikke forbinde", map[string]any{"Text": msg})
		return
	}
	ru, _ := url.Parse(p.RedirectURI)
	q := ru.Query()
	if p.State != "" {
		q.Set("state", p.State)
	}
	if r.PostForm.Get("decision") != "allow" {
		q.Set("error", "access_denied")
		ru.RawQuery = q.Encode()
		http.Redirect(w, r, ru.String(), http.StatusSeeOther)
		return
	}
	code := randomToken(24)
	_ = s.st.Do(func(db *DB) error {
		db.Codes[code] = &AuthCode{ClientID: p.ClientID, UserID: u.ID, RedirectURI: p.RedirectURI, Challenge: p.Challenge, Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	q.Set("code", code)
	ru.RawQuery = q.Encode()
	http.Redirect(w, r, ru.String(), http.StatusSeeOther)
}

// token bytter en autorisationskode til et adgangstoken (90 dage). Koden kan kun bruges én gang,
// og code_verifier skal passe til den gemte PKCE-challenge.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	var ac *AuthCode
	_ = s.st.Do(func(db *DB) error { // koden kan kun bruges én gang
		ac = db.Codes[r.PostForm.Get("code")]
		delete(db.Codes, r.PostForm.Get("code"))
		return nil
	})
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if ac == nil || time.Now().After(ac.Expires) ||
		ac.ClientID != r.PostForm.Get("client_id") || ac.RedirectURI != r.PostForm.Get("redirect_uri") ||
		subtle.ConstantTimeCompare([]byte(want), []byte(ac.Challenge)) != 1 {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	tok := "vdt_" + randomToken(32)
	_ = s.st.Do(func(db *DB) error {
		label := "AI-værktøj"
		if c := db.Clients[ac.ClientID]; c != nil && c.Name != "" {
			label = c.Name
		}
		db.Tokens[tok] = &Token{UserID: ac.UserID, ClientID: ac.ClientID, Kind: "oauth", Label: label, Created: time.Now(), Expires: time.Now().Add(90 * 24 * time.Hour)}
		return nil
	})
	writeJSON(w, 200, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 90 * 24 * 3600})
}

// cors tillader kald fra alle oprindelser til OAuth- og MCP-endpoints, som webbaserede AI-klienter
// kalder direkte fra browseren. De bruger Bearer-tokens, ikke cookies, så det åbner ikke for CSRF.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}
