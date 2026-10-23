package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// ---------- Ny app fra git, direkte i dashboardet ----------
//
// Samme vej som AI-værktøjet deploy_from_git (DeployFromGit): offentlige repos hentes direkte, og for et
// privat repo laver serveren en deploy key, som ejeren tilføjer i repoets indstillinger, hvorefter han
// prøver igen. Den nye app venter bagefter på ejerens godkendelse, som alle andre nye apps.

// gitNewForm er det, formularen viser: det ejeren har skrevet, en fejl og evt. en deploy key.
type gitNewForm struct {
	Name, Repo, Branch, Port string
	Err                      string
	Key                      *ErrNeedKey
}

// gitNewPage viser formularen «Ny app fra git».
func (s *Server) gitNewPage(w http.ResponseWriter, r *http.Request, u *User) {
	if !canBuild(u) {
		s.page(w, r, "message.html", "Ingen adgang", map[string]any{"Text": "Din konto kan åbne apps, der er delt med dig, men ikke bygge egne. Bed ejeren om tilladelse, hvis du vil.", "Back": "/"})
		return
	}
	s.page(w, r, "gitnew.html", "Ny app fra git", map[string]any{"F": gitNewForm{Port: "8080"}})
}

// repoDefaultName er et forslag til appens navn ud fra repoets navn, når ejeren ikke har skrevet et.
func repoDefaultName(repoIn string) string {
	repo, err := parseRepo(repoIn)
	if err != nil {
		return ""
	}
	return repo.Path[strings.LastIndex(repo.Path, "/")+1:]
}

// gitNewSubmit henter repoet og opretter appen. Fejl og «mangler deploy key» vises i formularen igen,
// så ejeren ikke skal skrive det hele forfra.
func (s *Server) gitNewSubmit(w http.ResponseWriter, r *http.Request, u *User) {
	if !canBuild(u) {
		http.Error(w, "Ingen adgang", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	f := gitNewForm{
		Name:   strings.TrimSpace(r.PostForm.Get("name")),
		Repo:   strings.TrimSpace(r.PostForm.Get("repo")),
		Branch: strings.TrimSpace(r.PostForm.Get("branch")),
		Port:   strings.TrimSpace(r.PostForm.Get("port")),
	}
	show := func() { s.page(w, r, "gitnew.html", "Ny app fra git", map[string]any{"F": f}) }
	if f.Repo == "" {
		f.Err = "Skriv, hvor koden ligger, fx ejer/repo eller https://github.com/ejer/repo."
		show()
		return
	}
	if f.Name == "" {
		f.Name = repoDefaultName(f.Repo)
	}
	port := 8080
	if f.Port != "" {
		p, err := strconv.Atoi(f.Port)
		if err != nil || p < 1 || p > 65535 {
			f.Err = "Porten skal være et tal mellem 1 og 65535, fx 8080. Det er den port, din app lytter på."
			show()
			return
		}
		port = p
	}
	if slug := slugify(f.Name); slug != "" {
		if _, taken := s.mgr.get(slug); taken {
			f.Err = "Der findes allerede en app med navnet «" + f.Name + "». Vælg et andet navn, eller åbn appen og brug «Hent nyeste version fra git»."
			show()
			return
		}
	}
	res, err := s.mgr.DeployFromGit(f.Name, f.Repo, f.Branch, port, "Hentet fra git via dashboardet", u.ID)
	var nk *ErrNeedKey
	switch {
	case errors.As(err, &nk):
		f.Key = nk
		show()
	case err != nil:
		f.Err = err.Error()
		show()
	default:
		http.Redirect(w, r, "/apps/"+res.App.Slug+"?m=gitnew", http.StatusSeeOther)
	}
}
