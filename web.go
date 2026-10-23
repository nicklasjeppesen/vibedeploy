package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Page er data til layout.html: titel, logget bruger, flash-besked og sidens egne data.
type Page struct {
	Title       string
	User        *User
	Msg         string
	Refresh     bool
	RefreshSecs int
	Data        any
}

// page tegner en side inde i layoutet. ?m=<kode> i adressen bliver til en kort besked øverst,
// og Refresh: true i data får siden til at opdatere sig selv hvert 3. sekund (RefreshSecs: n for et andet interval).
func (s *Server) page(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	p := Page{Title: title, User: s.currentUser(r), Data: data, Msg: flashText(r.URL.Query().Get("m")), RefreshSecs: 3}
	if m, ok := data.(map[string]any); ok {
		if b, ok := m["Refresh"].(bool); ok {
			p.Refresh = b
		}
		if n, ok := m["RefreshSecs"].(int); ok && n > 0 {
			p.Refresh, p.RefreshSecs = true, n
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		fmt.Println("template:", err)
	}
}

// flashText oversætter en kode fra ?m= til den besked, der vises efter en handling.
func flashText(code string) string {
	return map[string]string{
		"saved":        "Gemt. Genstart appen for at anvende ændringen.",
		"restart":      "Appen genstarter …",
		"approved":     "Godkendt. Appen bygges nu.",
		"stopped":      "Appen er sat på pause.",
		"token":        "",
		"revoked":      "Adgangen er fjernet for alle AI-værktøjer.",
		"shared":       "Deling gemt.",
		"backup":       "Backup taget.",
		"nodata":       "Appen har ingen data endnu, så der var intet at tage backup af.",
		"restored":     "Data gendannet. Den tidligere data blev gemt som en backup først.",
		"gitfetched":   "Nyeste version hentes fra git og bygges nu.",
		"pwreset":      "Din nye adgangskode er gemt, og du er logget ud alle andre steder.",
		"gituptodate":  "Appen kører allerede den nyeste version fra git. Der er ikke noget nyt at hente.",
		"gitchecked":   "Git er tjekket.",
		"gitcheckfail": "Git kunne ikke tjekkes. Se fejlen under «Hentes fra git».",
		"gitnew":       "Koden er hentet fra git. Se den igennem, og godkend den for at bygge og starte appen.",
		"rekeyed":      "Nøglen er slettet. Næste hentning laver en ny, som skal tilføjes i repoet igen.",
		"imagefetched": "Nyeste udgave af imaget hentes nu.",
		"updating":     "Opdateringen er startet. Vibe Deploy genstarter om lidt, og siden er væk et øjeblik; dine apps kører videre.",
		"checked":      "Tjekket for en ny version.",
		"autoon":       "Gemt. Nye versioner installeres automatisk på det valgte tidspunkt.",
		"autooff":      "Automatisk opdatering er slået fra.",
	}[code]
}

// statusLabel er den danske tekst for en app-status.
func statusLabel(st string) string {
	return map[string]string{StatusLive: "Live", StatusBuilding: "Bygger", StatusStarting: "Starter",
		StatusWaiting: "Venter på godkendelse", StatusStopped: "På pause", StatusFailed: "Fejlede"}[st]
}

// statusClass er CSS-klassen (farven) for en app-status.
func statusClass(st string) string {
	switch st {
	case StatusLive:
		return "ok"
	case StatusBuilding, StatusStarting, StatusWaiting:
		return "warn"
	case StatusFailed:
		return "bad"
	}
	return "idle"
}

// relTime viser et tidspunkt relativt til nu, fx «for 5 min. siden».
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "lige nu"
	case d < time.Hour:
		return fmt.Sprintf("for %d min. siden", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("for %d timer siden", int(d.Hours()))
	case d < 48*time.Hour:
		return "i går"
	}
	return fmt.Sprintf("for %d dage siden", int(d.Hours()/24))
}

// appURL er appens adresse: <slug>.<domæne> (med port på localhost).
func (s *Server) appURL(slug string) string {
	scheme := "http"
	if strings.HasPrefix(s.publicURL, "https://") {
		scheme = "https"
	}
	return scheme + "://" + slug + "." + s.domain + portSuffix(s.addr, s.domain)
}

// dashboardURL er dashboardets adresse, som den nævnes i beskeder til AI'en.
func (s *Server) dashboardURL(r *http.Request) string { return s.baseURL(r) }

// hostOnly fjerner porten fra en Host-header.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// handler opsætter alle ruter. Forespørgsler til <slug>.<domæne> sendes videre til appen (proxyApp);
// alt andet er dashboard, OAuth og MCP og får sikkerhedsheaders.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/{file}", s.static)
	mux.HandleFunc("GET /internal/tls-ask", s.tlsAsk)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /internal/reset-password", s.internalReset)
	mux.HandleFunc("GET /reset/{token}", s.resetPage)
	mux.HandleFunc("POST /reset/{token}", s.resetSubmit)
	mux.HandleFunc("GET /{$}", s.requireUser(s.dashboard))
	mux.HandleFunc("GET /signup", s.signupPage)
	mux.HandleFunc("POST /signup", s.signupSubmit)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /app-login", s.requireUser(s.appLogin))
	mux.HandleFunc("GET /people", s.requireAdmin(s.peoplePage))
	mux.HandleFunc("GET /server", s.requireAdmin(s.serverPage))
	mux.HandleFunc("POST /server/update", s.requireAdmin(s.serverUpdate))
	mux.HandleFunc("POST /people/invite", s.requireAdmin(s.peopleInvite))
	mux.HandleFunc("POST /people/invite/{token}/revoke", s.requireAdmin(s.peopleRevokeInvite))
	mux.HandleFunc("POST /people/user/{id}/reset", s.requireAdmin(s.peopleReset))
	mux.HandleFunc("POST /people/user/{id}/{action}", s.requireAdmin(s.peopleAction))
	mux.HandleFunc("GET /invite/{token}", s.invitePage)
	mux.HandleFunc("POST /invite/{token}", s.inviteSubmit)
	mux.HandleFunc("GET /connect", s.requireUser(s.connectPage))
	mux.HandleFunc("POST /connect/token", s.requireUser(s.connectToken))
	mux.HandleFunc("POST /connect/revoke", s.requireUser(s.connectRevoke))
	mux.HandleFunc("GET /apps/new/git", s.requireUser(s.gitNewPage))
	mux.HandleFunc("POST /apps/new/git", s.requireUser(s.gitNewSubmit))
	mux.HandleFunc("GET /apps/{slug}", s.requireUser(s.appPage))
	mux.HandleFunc("POST /apps/{slug}/{action}", s.requireUser(s.appAction))
	mux.HandleFunc("GET /authorize", s.requireUser(s.authorizePage))
	mux.HandleFunc("POST /authorize", s.requireUser(s.authorizeSubmit))

	mux.HandleFunc("/.well-known/oauth-protected-resource", cors(s.protectedResourceMeta))
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", cors(s.protectedResourceMeta))
	mux.HandleFunc("/.well-known/oauth-authorization-server", cors(s.authServerMeta))
	mux.HandleFunc("/register", cors(s.register))
	mux.HandleFunc("/token", cors(s.token))
	mux.HandleFunc("/mcp", cors(s.mcp))
	mux.HandleFunc("PUT /upload/{token}", s.upload)
	mux.HandleFunc("POST /upload/{token}", s.upload)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOnly(r.Host)
		if slug, ok := strings.CutSuffix(host, "."+s.domain); ok && slug != "" {
			s.proxyApp(w, r, slug)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self' http: https:")
		if s.domain != "localhost" && isIPHost(host) {
			// Besøg på serverens IP-adresse (fx under installationen): kun statussiden og dens CSS.
			// Alt andet sendes til domænet, så login aldrig sker over en ukrypteret IP-adresse.
			switch {
			case r.URL.Path == "/":
				s.setupStatus(w, r)
			case r.URL.Path == "/healthz": // opdateringen spørger på 127.0.0.1
				mux.ServeHTTP(w, r)
			case r.URL.Path == "/internal/tls-ask": // Caddy spørger på 127.0.0.1, før den henter et certifikat til en app
				mux.ServeHTTP(w, r)
			case r.URL.Path == "/internal/reset-password": // kommandoen «vibedeploy reset-password» spørger på 127.0.0.1
				mux.ServeHTTP(w, r)
			case strings.HasPrefix(r.URL.Path, "/static/"):
				mux.ServeHTTP(w, r)
			default:
				http.Redirect(w, r, "https://"+s.domain+r.URL.RequestURI(), http.StatusSeeOther)
			}
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// static serverer style.css og app.js fra de indlejrede filer.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	f := r.PathValue("file")
	if f != "style.css" && f != "app.js" {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(webFS, "web/"+f)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(f, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Write(b)
}

// proxyApp sender en forespørgsel på appens adresse videre til containeren på 127.0.0.1:<HostPort>.
// Ikke-offentlige apps kræver en app-session; ellers sendes den besøgende til login i dashboardet.
// Vores egen cookie fjernes, før forespørgslen når appen. Kører appen ikke, vises en ventende side,
// der opdaterer sig selv.
func (s *Server) proxyApp(w http.ResponseWriter, r *http.Request, slug string) {
	app, ok := s.mgr.get(slug)
	if !ok {
		http.Error(w, "Der findes ingen app på denne adresse.", http.StatusNotFound)
		return
	}
	if r.URL.Path == "/_vd/auth" {
		s.appAuth(w, r, slug)
		return
	}
	if app.Visibility != VisPublic && !s.appSessionOK(r, &app) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, s.dashBase()+"/app-login?app="+url.QueryEscape(slug)+"&next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "Log ind for at åbne appen.", http.StatusUnauthorized)
		return
	}
	stripCookie(r, appCookie)
	if !serving(app) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><meta http-equiv=refresh content=5><title>%s</title>
<body style="font:18px system-ui;max-width:560px;margin:18vh auto;padding:0 20px"><h1>%s</h1><p>%s. Siden opdaterer sig selv.</p></body>`,
			template_escape(app.Name), template_escape(app.Name), template_escape(statusLabel(app.Status)))
		return
	}
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(app.HostPort))
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Appen svarer ikke lige nu.", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

// serving afgør, om appens trafik skal sendes videre til containeren. Mens en ny version bygges
// eller starter, kører den forrige stadig (udrulning uden nedetid), så besøgende sendes til den.
func serving(a App) bool {
	switch a.Status {
	case StatusLive:
		return true
	case StatusBuilding, StatusStarting:
		return a.Current > 0
	}
	return false
}

// template_escape HTML-escaper s til den simple ventende side i proxyApp.
func template_escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// ---------- visningsmodeller ----------

// AppView er en app plus de felter, dashboardets skabeloner viser.
type AppView struct {
	App
	URL, Label, Class, Ago string
	Missing                []string
	VisLabel, Owner        string
	Manage                 bool
}

// view laver en AppView med adresse, statustekst og manglende hemmeligheder.
func (s *Server) view(a App) AppView {
	v := AppView{App: a, URL: s.appURL(a.Slug), Label: statusLabel(a.Status), Class: statusClass(a.Status), Ago: relTime(a.Updated), VisLabel: visLabel(a.Visibility)}
	for _, sec := range a.Secrets {
		if sec.Value == "" {
			v.Missing = append(v.Missing, sec.Key)
		}
	}
	return v
}

// ---------- konto ----------

// signupPage viser oprettelse af den første konto (ejeren). Findes der brugere, sendes man til login.
func (s *Server) signupPage(w http.ResponseWriter, r *http.Request) {
	if len(s.st.Snapshot().Users) > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.page(w, r, "signup.html", "Opret konto", map[string]any{"NeedCode": s.needSetupCode()})
}

// needSetupCode er sand, når første konto kræver opsætningskoden fra serverens log.
func (s *Server) needSetupCode() bool {
	s.codeMu.Lock()
	defer s.codeMu.Unlock()
	return s.setupCode != ""
}

// checkSetupCode tjekker opsætningskoden i konstant tid. Er der ingen kode (localhost), accepteres alt.
func (s *Server) checkSetupCode(given string) bool {
	s.codeMu.Lock()
	defer s.codeMu.Unlock()
	return s.setupCode == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(given)), []byte(s.setupCode)) == 1
}

// signupSubmit opretter ejerkontoen. Kun den første konto kan oprettes her;
// alle andre kommer ind via invitationer.
func (s *Server) signupSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Forbudt", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	name, email, pw := strings.TrimSpace(r.PostForm.Get("name")), strings.TrimSpace(r.PostForm.Get("email")), r.PostForm.Get("password")
	fail := func(msg string) {
		s.page(w, r, "signup.html", "Opret konto", map[string]any{"Err": msg, "Name": name, "Email": email, "NeedCode": s.needSetupCode()})
	}
	if !s.checkSetupCode(r.PostForm.Get("setupcode")) {
		time.Sleep(600 * time.Millisecond)
		fail("Opsætningskoden er forkert. Den står i serverens log, hvor programmet blev startet.")
		return
	}
	if name == "" || !strings.Contains(email, "@") {
		fail("Skriv dit navn og en gyldig e-mail.")
		return
	}
	if len([]rune(pw)) < 12 {
		fail("Adgangskoden skal være mindst 12 tegn.")
		return
	}
	var uid string
	err := s.st.Do(func(db *DB) error { // kun den første bruger kan oprettes (ejeren)
		if len(db.Users) > 0 {
			return http.ErrAbortHandler
		}
		salt := randomToken(16)
		uid = randomToken(8)
		db.Users = append(db.Users, &User{ID: uid, Email: email, Name: name, Salt: salt, Hash: hashPassword(pw, salt), Role: RoleOwner, Builder: true, Created: time.Now()})
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.codeMu.Lock()
	s.setupCode = ""
	s.codeMu.Unlock()
	if err := s.startSession(w, r, uid); err != nil {
		http.Error(w, "Kunne ikke starte session", 500)
		return
	}
	http.Redirect(w, r, "/connect", http.StatusSeeOther)
}

// loginPage viser login-formularen (eller sender til signup, hvis der endnu ingen konto er).
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if len(s.st.Snapshot().Users) == 0 {
		http.Redirect(w, r, "/signup", http.StatusSeeOther)
		return
	}
	s.page(w, r, "login.html", "Log ind", map[string]any{"Next": safeNext(r.URL.Query().Get("next"))})
}

// loginSubmit logger ind med e-mail og adgangskode og sender brugeren videre til next.
// Forkerte forsøg venter 600 ms for at gøre gætteri langsommere.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Forbudt", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	email, pw, next := strings.TrimSpace(r.PostForm.Get("email")), r.PostForm.Get("password"), safeNext(r.PostForm.Get("next"))
	var found *User
	for _, u := range s.st.Snapshot().Users {
		if strings.EqualFold(u.Email, email) {
			found = u
		}
	}
	if found == nil || !checkPassword(found, pw) {
		time.Sleep(600 * time.Millisecond) // gør gætteri langsommere
		s.page(w, r, "login.html", "Log ind", map[string]any{"Err": "Forkert e-mail eller adgangskode.", "Next": next})
		return
	}
	if err := s.startSession(w, r, found.ID); err != nil {
		http.Error(w, "Kunne ikke starte session", 500)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout sletter dashboard-sessionen og brugerens app-sessioner og fjerner cookien.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && sameOrigin(r) {
		_ = s.st.Do(func(db *DB) error {
			if sess := db.Sessions[c.Value]; sess != nil {
				for k, x := range db.Sessions {
					if x.UserID == sess.UserID && x.AppSlug != "" {
						delete(db.Sessions, k)
					}
				}
			}
			delete(db.Sessions, c.Value)
			return nil
		})
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- dashboard ----------

// dashboard er forsiden: de apps, brugeren må åbne, og en kort opsummering af deres status.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, u *User) {
	db := s.st.Snapshot()
	var views []AppView
	live, busy, waiting := 0, 0, 0
	for _, a := range db.Apps {
		if !canOpen(a, u) {
			continue
		}
		v := s.view(*a)
		v.Manage = canManage(a, u)
		if ow := db.user(a.OwnerID); ow != nil {
			v.Owner = ow.Name
		}
		views = append(views, v)
		switch a.Status {
		case StatusLive:
			live++
		case StatusBuilding, StatusStarting:
			busy++
		case StatusWaiting:
			waiting++
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	var parts []string
	if live > 0 {
		parts = append(parts, fmt.Sprintf("%d %s live", live, plural(live, "app er", "apps er")))
	}
	if busy > 0 {
		parts = append(parts, fmt.Sprintf("%d er ved at starte", busy))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d venter på din godkendelse", waiting))
	}
	connected := false
	for _, t := range db.Tokens {
		if t.UserID == u.ID && time.Now().Before(t.Expires) {
			connected = true
		}
	}
	alert := ""
	if u.IsAdmin() {
		alert = s.serverAlert()
	}
	s.page(w, r, "home.html", "Dine apps", map[string]any{
		"Apps": views, "Summary": strings.Join(parts, ". "), "Connected": connected, "Refresh": busy > 0, "CanBuild": canBuild(u),
		"Alert": alert,
	})
}

// plural vælger ental eller flertal ud fra n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------- forbind AI ----------

// connectPage viser siden «Forbind din AI».
func (s *Server) connectPage(w http.ResponseWriter, r *http.Request, u *User) {
	s.renderConnect(w, r, u, "")
}

// renderConnect viser MCP-adressen og brugerens aktive AI-adgange.
// newToken er en netop oprettet adgangsnøgle, som kun vises denne ene gang.
func (s *Server) renderConnect(w http.ResponseWriter, r *http.Request, u *User, newToken string) {
	if !canBuild(u) {
		s.page(w, r, "message.html", "Ingen adgang", map[string]any{"Text": "Din konto kan åbne apps, der er delt med dig, men ikke bygge egne. Bed ejeren om tilladelse, hvis du vil.", "Back": "/"})
		return
	}
	db := s.st.Snapshot()
	type tk struct{ Label, Kind, Ago string }
	var toks []tk
	for _, t := range db.Tokens {
		if t.UserID == u.ID && time.Now().Before(t.Expires) {
			toks = append(toks, tk{t.Label, t.Kind, relTime(t.Created)})
		}
	}
	s.page(w, r, "connect.html", "Forbind din AI", map[string]any{
		"MCPURL": s.baseURL(r) + "/mcp", "Tokens": toks, "Connected": len(toks) > 0, "NewToken": newToken,
		"Local": s.publicURL == "" && strings.Contains(r.Host, "localhost"),
	})
}

// connectToken opretter en personlig adgangsnøgle (gyldig i et år) til AI-værktøjer,
// der ikke kan logge ind med OAuth.
func (s *Server) connectToken(w http.ResponseWriter, r *http.Request, u *User) {
	if !canBuild(u) {
		http.Error(w, "Ingen adgang", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	label := strings.TrimSpace(r.PostForm.Get("label"))
	if label == "" || len(label) > 60 {
		label = "Adgangsnøgle"
	}
	tok := "vdt_" + randomToken(32)
	_ = s.st.Do(func(db *DB) error {
		db.Tokens[tok] = &Token{UserID: u.ID, Kind: "pat", Label: label, Created: time.Now(), Expires: time.Now().Add(365 * 24 * time.Hour)}
		return nil
	})
	s.renderConnect(w, r, u, tok)
}

// connectRevoke fjerner alle brugerens AI-adgange (både OAuth-tokens og adgangsnøgler).
func (s *Server) connectRevoke(w http.ResponseWriter, r *http.Request, u *User) {
	_ = s.st.Do(func(db *DB) error {
		for k, t := range db.Tokens {
			if t.UserID == u.ID {
				delete(db.Tokens, k)
			}
		}
		return nil
	})
	http.Redirect(w, r, "/connect?m=revoked", http.StatusSeeOther)
}

// ---------- app ----------

// appPage viser en apps side med fanerne oversigt, versioner, log, indstillinger og data.
// Kun appens ejer og serverens ejer har adgang.
func (s *Server) appPage(w http.ResponseWriter, r *http.Request, u *User) {
	a, ok := s.mgr.get(r.PathValue("slug"))
	if !ok {
		s.page(w, r, "message.html", "Findes ikke", map[string]any{"Text": "Den app findes ikke (mere)."})
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "versioner" && tab != "log" && tab != "indstillinger" && tab != "data" {
		tab = "oversigt"
	}
	if !canManage(&a, u) {
		s.page(w, r, "message.html", "Ingen adgang", map[string]any{"Text": "Kun appens ejer kan styre den. Du kan åbne den fra forsiden, hvis den er delt med dig.", "Back": "/"})
		return
	}
	v := s.view(a)
	type ver struct {
		Version
		Ago     string
		Current bool
		CanBack bool
	}
	var vers []ver
	for i := len(a.Versions) - 1; i >= 0; i-- {
		x := a.Versions[i]
		vers = append(vers, ver{x, relTime(x.Created), x.N == a.Current, x.Image != "" && x.N != a.Current})
	}
	data := map[string]any{"A": v, "Tab": tab, "Versions": vers, "Refresh": a.Status == StatusBuilding || a.Status == StatusStarting,
		"Prompt": fmt.Sprintf("Min app «%s» har et problem. Hent loggen med get_logs, find fejlen og udgiv en rettelse.", a.Name),
		"Usage":  s.appUsage(a.Slug)}
	if tab == "log" {
		data["Log"] = s.mgr.Logs(a.Slug, 200)
	}
	if a.Git != nil {
		data["GitNew"] = gitState(a)["NewOnGit"]
	}
	if tab == "data" {
		data["Backups"] = s.mgr.Backups(a.Slug)
		empty, _ := dirEmpty(s.mgr.volumeDir(a.Slug))
		data["HasData"] = !empty
	}
	if tab == "indstillinger" {
		data["EnvRows"] = a.Env
		type person struct {
			ID, Name string
			Checked  bool
		}
		var people []person
		for _, p := range s.st.Snapshot().Users {
			if p.ID == a.OwnerID || p.IsAdmin() {
				continue
			}
			ck := false
			for _, id := range a.Access {
				ck = ck || id == p.ID
			}
			people = append(people, person{p.ID, p.Name, ck})
		}
		data["People"] = people
		var gk *GitKey
		s.st.View(func(db *DB) {
			if k := db.GitKeys[a.Slug]; k != nil {
				cp := *k
				gk = &cp
			}
		})
		if a.Git != nil || gk != nil {
			g := map[string]any{}
			if a.Git != nil {
				g["Repo"], g["Branch"] = a.Git.Repo, a.Git.Branch
				for k, v := range gitState(a) {
					g[k] = v
				}
			}
			if gk != nil {
				g["Public"] = gk.Public
				if repo, err := parseRepo(gk.Repo); err == nil {
					g["AddURL"] = repo.AddKeyURL()
					if a.Git == nil {
						g["Repo"] = gk.Repo
					}
				}
			}
			data["Git"] = g
		}
	}
	s.page(w, r, "app.html", a.Name, data)
}

// gitState er det, appens side viser om git: kørende og nyeste commit, om der er noget nyt, og automatikken.
func gitState(a App) map[string]any {
	g := a.Git
	cur := currentCommit(a)
	st := map[string]any{"Commit": shortSHA(cur), "Latest": shortSHA(g.Latest), "CheckErr": g.CheckErr}
	if !g.Checked.IsZero() {
		st["Checked"] = relTime(g.Checked)
	}
	st["UpToDate"] = cur != "" && g.Latest == cur
	st["NewOnGit"] = g.Latest != "" && g.Latest != cur && a.Current > 0
	return st
}

// appAction håndterer knapperne på appens side (godkend, afvis, genstart, stop, rollback,
// variabler, hemmeligheder, backup/gendan, git, deling og slet) og sender tilbage til den relevante fane.
func (s *Server) appAction(w http.ResponseWriter, r *http.Request, u *User) {
	slug, action := r.PathValue("slug"), r.PathValue("action")
	_ = r.ParseForm()
	if ap, found := s.mgr.get(slug); !found || !canManage(&ap, u) {
		s.page(w, r, "message.html", "Ingen adgang", map[string]any{"Text": "Du kan ikke styre den app.", "Back": "/"})
		return
	}
	back := func(tab, m string) {
		http.Redirect(w, r, "/apps/"+slug+"?tab="+tab+"&m="+m, http.StatusSeeOther)
	}
	fail := func(err error) {
		s.page(w, r, "message.html", "Det lykkedes ikke", map[string]any{"Text": err.Error(), "Back": "/apps/" + slug})
	}
	var err error
	switch action {
	case "approve":
		if err = s.mgr.Approve(slug); err == nil {
			back("oversigt", "approved")
			return
		}
	case "reject":
		if err = s.mgr.Reject(slug); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	case "restart":
		if err = s.mgr.Restart(slug); err == nil {
			back("oversigt", "restart")
			return
		}
	case "stop":
		if err = s.mgr.Stop(slug); err == nil {
			back("oversigt", "stopped")
			return
		}
	case "rollback":
		n, _ := strconv.Atoi(r.PostForm.Get("version"))
		if err = s.mgr.Rollback(slug, n); err == nil {
			back("oversigt", "restart")
			return
		}
	case "env":
		keys, vals := r.PostForm["key"], r.PostForm["value"]
		var vars []KV
		for i := range keys {
			k := strings.TrimSpace(keys[i])
			if k == "" || i >= len(vals) {
				continue
			}
			vars = append(vars, KV{k, vals[i]})
		}
		if err = s.mgr.SetEnv(slug, vars); err == nil {
			back("indstillinger", "saved")
			return
		}
	case "secret":
		key, val := strings.TrimSpace(r.PostForm.Get("key")), r.PostForm.Get("value")
		if val == "" {
			err = fmt.Errorf("skriv en værdi, før du gemmer")
		} else if err = s.mgr.SetSecret(slug, key, val); err == nil {
			back("indstillinger", "saved")
			return
		}
	case "newsecret": // variabelnavn og evt. værdi; uden værdi oprettes en tom plads
		key, val := strings.TrimSpace(r.PostForm.Get("key")), r.PostForm.Get("value")
		if val != "" {
			err = s.mgr.SetSecret(slug, key, val)
		} else {
			err = s.mgr.RequestSecrets(slug, []string{key})
		}
		if err == nil {
			back("indstillinger", "saved")
			return
		}
	case "backup":
		var name string
		if name, err = s.mgr.Backup(slug, "manuel"); err == nil {
			if name == "" {
				back("data", "nodata")
			} else {
				back("data", "backup")
			}
			return
		}
	case "restore":
		if err = s.mgr.Restore(slug, r.PostForm.Get("name")); err == nil {
			back("data", "restored")
			return
		}
	case "gitdeploy":
		ap, _ := s.mgr.get(slug)
		if ap.Git == nil {
			err = fmt.Errorf("appen er ikke hentet fra git")
			break
		}
		// Er der ikke noget nyt på git, bygges der ikke (medmindre ejeren beder om det med «Byg igen»).
		if r.PostForm.Get("force") != "1" {
			if sha, cerr := s.mgr.CheckGit(slug); cerr == nil && sha != "" && sha == currentCommit(ap) && ap.Status == StatusLive {
				back("indstillinger", "gituptodate")
				return
			}
		}
		var res *DeployResult
		res, err = s.mgr.DeployFromGit(ap.Name, ap.Git.Repo, ap.Git.Branch, ap.Port, "Hentet fra git via dashboardet", ap.OwnerID)
		var nk *ErrNeedKey
		if errors.As(err, &nk) {
			s.page(w, r, "message.html", "Kunne ikke hente fra git", map[string]any{"Text": needKeyText(nk), "Back": "/apps/" + slug + "?tab=indstillinger"})
			return
		}
		if err == nil {
			_ = res
			back("oversigt", "gitfetched")
			return
		}
	case "imagedeploy":
		ap, _ := s.mgr.get(slug)
		if ap.FromImage == "" {
			err = fmt.Errorf("appen er ikke et færdigt image")
			break
		}
		if _, err = s.mgr.DeployImage(ap.Name, ap.FromImage, ap.Port, false, "Hentet igen via dashboardet", ap.OwnerID); err == nil {
			back("oversigt", "imagefetched")
			return
		}
	case "gitcheck":
		if _, cerr := s.mgr.CheckGit(slug); cerr != nil {
			back("indstillinger", "gitcheckfail")
			return
		}
		back("indstillinger", "gitchecked")
		return
	case "gitrekey":
		s.mgr.RemoveKey(slug)
		back("indstillinger", "rekeyed")
		return
	case "access":
		if err = s.mgr.SetAccess(slug, r.PostForm.Get("visibility"), r.PostForm["user"]); err == nil {
			back("indstillinger", "shared")
			return
		}
	case "delete":
		a, ok := s.mgr.get(slug)
		if !ok || strings.TrimSpace(r.PostForm.Get("confirm")) != a.Name {
			err = fmt.Errorf("skriv appens navn nøjagtigt for at bekræfte sletningen")
		} else if err = s.mgr.Delete(slug); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	fail(err)
}

// tlsAsk bruges af Caddy ("on-demand TLS"): kun adresser, der hører til serveren, får et certifikat.
// Kun kald direkte fra samme maskine accepteres; kald gennem proxyen (med X-Forwarded-For) afvises.
func (s *Server) tlsAsk(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || r.Header.Get("X-Forwarded-For") != "" {
		http.NotFound(w, r)
		return
	}
	d := strings.ToLower(r.URL.Query().Get("domain"))
	if d == s.domain {
		w.WriteHeader(http.StatusOK)
		return
	}
	if slug, ok := strings.CutSuffix(d, "."+s.domain); ok {
		if _, found := s.mgr.get(slug); found {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	http.NotFound(w, r)
}
