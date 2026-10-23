package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	RoleOwner  = "owner"
	RoleMember = "member"

	VisPrivate = "private" // kun ejeren (og administratoren)
	VisShared  = "shared"  // udvalgte personer
	VisMembers = "members" // alle med konto på serveren
	VisPublic  = "public"  // alle på internettet, uden login
)

// IsAdmin er sand for serverens ejer, som kan se og styre alt.
func (u *User) IsAdmin() bool { return u.Role == RoleOwner }

// canBuild: må forbinde en AI og udgive egne apps.
func canBuild(u *User) bool { return u.IsAdmin() || u.Builder }

// canManage: må styre appen (indstillinger, versioner, hemmeligheder, deling).
func canManage(a *App, u *User) bool { return u.IsAdmin() || a.OwnerID == u.ID }

// canOpen: må åbne selve appen.
func canOpen(a *App, u *User) bool {
	if canManage(a, u) {
		return true
	}
	switch a.Visibility {
	case VisMembers:
		return true
	case VisShared:
		for _, id := range a.Access {
			if id == u.ID {
				return true
			}
		}
	case VisPublic:
		return true
	}
	return false
}

// visLabel giver den danske tekst for en synlighed, som den vises i dashboardet.
func visLabel(v string) string {
	switch v {
	case VisShared:
		return "Udvalgte personer"
	case VisMembers:
		return "Alle med konto"
	case VisPublic:
		return "Offentlig"
	}
	return "Kun mig"
}

// ---------- adgang til selve appene (login foran app-adressen) ----------

const appCookie = "vd_app"

// dashBase er dashboardets adresse, som app-adresserne sender folk til for at logge ind.
func (s *Server) dashBase() string {
	if s.publicURL != "" {
		return s.publicURL
	}
	return "http://" + s.domain + portSuffix(s.addr, s.domain)
}

// appSecure fortæller, om appens adresse er https, så app-cookien kan markeres Secure.
func (s *Server) appSecure(slug string) bool { return strings.HasPrefix(s.appURL(slug), "https://") }

// appSessionOK: har brugeren en gyldig app-session, og må brugeren stadig åbne appen?
func (s *Server) appSessionOK(r *http.Request, app *App) bool {
	c, err := r.Cookie(appCookie)
	if err != nil {
		return false
	}
	ok := false
	s.st.View(func(db *DB) {
		sess := db.Sessions[c.Value]
		if sess == nil || sess.AppSlug != app.Slug || time.Now().After(sess.Expires) {
			return
		}
		if u := db.user(sess.UserID); u != nil && canOpen(app, u) {
			ok = true
		}
	})
	return ok
}

// appLogin: dashboardet bekræfter, at brugeren må åbne appen, og sender en engangsbillet videre.
func (s *Server) appLogin(w http.ResponseWriter, r *http.Request, u *User) {
	slug := r.URL.Query().Get("app")
	next := safeNext(r.URL.Query().Get("next"))
	app, ok := s.mgr.get(slug)
	if !ok {
		s.page(w, r, "message.html", "Findes ikke", map[string]any{"Text": "Den app findes ikke (mere).", "Back": "/"})
		return
	}
	if !canOpen(&app, u) {
		s.page(w, r, "message.html", "Ingen adgang", map[string]any{
			"Text": fmt.Sprintf("Du har ikke adgang til «%s». Bed ejeren om at dele den med dig.", app.Name), "Back": "/"})
		return
	}
	t := randomToken(16)
	_ = s.st.Do(func(db *DB) error {
		for k, tk := range db.Tickets {
			if time.Now().After(tk.Expires) {
				delete(db.Tickets, k)
			}
		}
		db.Tickets[t] = &Ticket{UserID: u.ID, Slug: slug, Expires: time.Now().Add(60 * time.Second)}
		return nil
	})
	http.Redirect(w, r, s.appURL(slug)+"/_vd/auth?t="+t+"&next="+url.QueryEscape(next), http.StatusSeeOther)
}

// appAuth kører på app-adressen: bytter engangsbilletten til en cookie, der kun gælder for den app.
func (s *Server) appAuth(w http.ResponseWriter, r *http.Request, slug string) {
	t, next := r.URL.Query().Get("t"), safeNext(r.URL.Query().Get("next"))
	var uid string
	_ = s.st.Do(func(db *DB) error {
		tk := db.Tickets[t]
		delete(db.Tickets, t) // kan kun bruges én gang
		if tk != nil && tk.Slug == slug && time.Now().Before(tk.Expires) {
			uid = tk.UserID
		}
		return nil
	})
	app, ok := s.mgr.get(slug)
	var user *User
	if uid != "" && ok {
		s.st.View(func(db *DB) {
			if u := db.user(uid); u != nil {
				cp := *u
				user = &cp
			}
		})
	}
	if user == nil || !canOpen(&app, user) {
		http.Redirect(w, r, s.dashBase()+"/app-login?app="+url.QueryEscape(slug)+"&next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	tok := randomToken(32)
	_ = s.st.Do(func(db *DB) error {
		db.Sessions[tok] = &Session{UserID: uid, AppSlug: slug, Expires: time.Now().Add(7 * 24 * time.Hour)}
		return nil
	})
	http.SetCookie(w, &http.Cookie{Name: appCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.appSecure(slug), MaxAge: 7 * 24 * 3600})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// stripCookie fjerner vores egen cookie, så appen aldrig ser den.
func stripCookie(r *http.Request, name string) {
	cs := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cs {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

// ---------- deling pr. app ----------

// SetAccess ændrer en apps synlighed. Ved «udvalgte personer» gemmes kun brugere, der findes,
// og appens ejer springes over (ejeren har altid adgang).
func (m *Manager) SetAccess(slug, vis string, ids []string) error {
	switch vis {
	case VisPrivate, VisShared, VisMembers, VisPublic:
	default:
		return fmt.Errorf("ukendt synlighed")
	}
	return m.st.Do(func(db *DB) error {
		a, ok := db.Apps[slug]
		if !ok {
			return fmt.Errorf("appen findes ikke")
		}
		var keep []string
		if vis == VisShared {
			for _, id := range ids {
				if u := db.user(id); u != nil && u.ID != a.OwnerID {
					keep = append(keep, id)
				}
			}
		}
		a.Visibility, a.Access, a.Updated = vis, keep, time.Now()
		return nil
	})
}

// ---------- personer og invitationer (kun administratoren) ----------

// requireAdmin er som requireUser, men lukker kun serverens ejer ind.
func (s *Server) requireAdmin(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request, u *User) {
		if !u.IsAdmin() {
			s.page(w, r, "message.html", "Ingen adgang", map[string]any{"Text": "Kun serverens ejer kan se denne side.", "Back": "/"})
			return
		}
		h(w, r, u)
	})
}

// peoplePage viser siden Personer.
func (s *Server) peoplePage(w http.ResponseWriter, r *http.Request, u *User) {
	s.renderPeople(w, r, "")
}

// renderPeople viser brugere, antal apps pr. bruger og aktive invitationer.
// newLink er et netop oprettet invitationslink, som kun vises denne ene gang.
func (s *Server) renderPeople(w http.ResponseWriter, r *http.Request, newLink string) {
	s.renderPeopleWith(w, r, map[string]any{"NewLink": newLink})
}

// renderPeopleWith viser siden Personer med ekstra felter, fx et netop lavet link til ny adgangskode.
func (s *Server) renderPeopleWith(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	db := s.st.Snapshot()
	type invite struct {
		Token   string
		Builder bool
		Ago     string
	}
	var invs []invite
	for t, iv := range db.Invites {
		if time.Now().Before(iv.Expires) {
			invs = append(invs, invite{t, iv.Builder, relTime(iv.Created)})
		}
	}
	apps := map[string]int{}
	for _, a := range db.Apps {
		apps[a.OwnerID]++
	}
	type person struct {
		*User
		Apps int
	}
	var people []person
	for _, p := range db.Users {
		people = append(people, person{p, apps[p.ID]})
	}
	data := map[string]any{"People": people, "Invites": invs}
	for k, v := range extra {
		data[k] = v
	}
	s.page(w, r, "people.html", "Personer", data)
}

// peopleInvite opretter et invitationslink (virker én gang i 7 dage) og viser det.
func (s *Server) peopleInvite(w http.ResponseWriter, r *http.Request, u *User) {
	_ = r.ParseForm()
	tok := randomToken(16)
	_ = s.st.Do(func(db *DB) error {
		db.Invites[tok] = &Invite{Builder: r.PostForm.Get("builder") == "on", Created: time.Now(), Expires: time.Now().Add(7 * 24 * time.Hour)}
		return nil
	})
	s.renderPeople(w, r, s.baseURL(r)+"/invite/"+tok)
}

// peopleRevokeInvite sletter en ubrugt invitation.
func (s *Server) peopleRevokeInvite(w http.ResponseWriter, r *http.Request, u *User) {
	_ = s.st.Do(func(db *DB) error { delete(db.Invites, r.PathValue("token")); return nil })
	http.Redirect(w, r, "/people", http.StatusSeeOther)
}

// peopleAction slår en persons bygge-ret til/fra («builder») eller fjerner personen («remove»).
// Fjernes bygge-retten, slettes personens AI-adgange med det samme. Fjernes personen, flyttes
// vedkommendes apps til ejeren og bliver private. Ejeren selv kan ikke ændres her.
func (s *Server) peopleAction(w http.ResponseWriter, r *http.Request, u *User) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var msg string
	err := s.st.Do(func(db *DB) error {
		t := db.user(id)
		if t == nil || t.IsAdmin() {
			msg = "Ejeren kan ikke ændres her."
			return nil
		}
		switch action {
		case "builder":
			t.Builder = !t.Builder
			if !t.Builder { // fjern AI-adgang med det samme
				for k, tk := range db.Tokens {
					if tk.UserID == t.ID {
						delete(db.Tokens, k)
					}
				}
			}
		case "remove":
			for i, x := range db.Users {
				if x.ID == id {
					db.Users = append(db.Users[:i], db.Users[i+1:]...)
					break
				}
			}
			for k, sess := range db.Sessions {
				if sess.UserID == id {
					delete(db.Sessions, k)
				}
			}
			for k, tk := range db.Tokens {
				if tk.UserID == id {
					delete(db.Tokens, k)
				}
			}
			for _, a := range db.Apps { // deres apps flyttes til ejeren og bliver private
				if a.OwnerID == id {
					a.OwnerID, a.Visibility, a.Access = u.ID, VisPrivate, nil
				}
				var keep []string
				for _, x := range a.Access {
					if x != id {
						keep = append(keep, x)
					}
				}
				a.Access = keep
			}
		}
		return nil
	})
	if err != nil || msg != "" {
		s.page(w, r, "message.html", "Det lykkedes ikke", map[string]any{"Text": msg, "Back": "/people"})
		return
	}
	http.Redirect(w, r, "/people", http.StatusSeeOther)
}

// invitePage viser formularen til at oprette en konto fra et invitationslink.
func (s *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	if !s.inviteValid(r.PathValue("token")) {
		s.page(w, r, "message.html", "Invitationen virker ikke", map[string]any{"Text": "Linket er udløbet eller brugt. Bed ejeren om en ny invitation.", "Back": "/login"})
		return
	}
	s.page(w, r, "invite.html", "Opret din konto", map[string]any{})
}

// inviteValid er sand, hvis invitationen findes og ikke er udløbet.
func (s *Server) inviteValid(tok string) bool {
	ok := false
	s.st.View(func(db *DB) {
		iv := db.Invites[tok]
		ok = iv != nil && time.Now().Before(iv.Expires)
	})
	return ok
}

// inviteSubmit opretter en medlemskonto ud fra en invitation, bruger invitationen op og logger personen ind.
func (s *Server) inviteSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Forbudt", http.StatusForbidden)
		return
	}
	tok := r.PathValue("token")
	_ = r.ParseForm()
	name, email, pw := strings.TrimSpace(r.PostForm.Get("name")), strings.TrimSpace(r.PostForm.Get("email")), r.PostForm.Get("password")
	fail := func(msg string) {
		s.page(w, r, "invite.html", "Opret din konto", map[string]any{"Err": msg, "Name": name, "Email": email})
	}
	if !s.inviteValid(tok) {
		s.invitePage(w, r)
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
	var uid, errMsg string
	_ = s.st.Do(func(db *DB) error {
		iv := db.Invites[tok]
		if iv == nil || time.Now().After(iv.Expires) {
			errMsg = "Invitationen er udløbet."
			return nil
		}
		for _, x := range db.Users {
			if strings.EqualFold(x.Email, email) {
				errMsg = "Der findes allerede en konto med den e-mail."
				return nil
			}
		}
		salt := randomToken(16)
		uid = randomToken(8)
		db.Users = append(db.Users, &User{ID: uid, Email: email, Name: name, Salt: salt, Hash: hashPassword(pw, salt), Role: RoleMember, Builder: iv.Builder, Created: time.Now()})
		delete(db.Invites, tok) // kan kun bruges én gang
		return nil
	})
	if errMsg != "" {
		fail(errMsg)
		return
	}
	if err := s.startSession(w, r, uid); err != nil {
		http.Error(w, "Kunne ikke starte session", 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
