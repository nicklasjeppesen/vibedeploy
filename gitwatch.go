package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ---------- Ny version på git? ----------
//
// For hver app fra git huskes, hvilken commit hver version er bygget fra (Version.Commit). Når ejeren trykker
// «Tjek git nu» eller «Hent nyeste version fra git», spørger serveren git med «git ls-remote», hvad den nyeste
// commit på grenen er. Det henter ikke koden, så det er hurtigt. Så kan dashboardet vise, om appen kører den
// nyeste version, og hent-knappen bygger ikke, når der ikke er noget nyt. Serveren tjekker aldrig git af sig
// selv, og der udrulles aldrig noget, uden at ejeren har bedt om det.

// shaRe er en fuld git-commit (SHA-1 eller SHA-256).
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// shortSHA er de første 7 tegn af en commit, som GitHub viser dem.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// parseLsRemote finder commit'en i output fra «git ls-remote <repo> <ref>».
func parseLsRemote(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && shaRe.MatchString(f[0]) {
			return f[0], nil
		}
	}
	return "", errors.New("grenen findes ikke i repoet")
}

// remoteHead spørger repoURL om den nyeste commit på branch (standardgrenen, hvis tom). key er en
// SSH-nøgle til private repos (tom for offentlige).
func (m *Manager) remoteHead(repoURL, branch, key string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git er ikke installeret på serveren (apt install git)")
	}
	ref := "HEAD"
	if branch != "" {
		ref = "refs/heads/" + branch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always",
		"ls-remote", "--", repoURL, ref)
	cmd.Env = m.gitEnv(key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New(lastLines(string(out), 2))
	}
	return parseLsRemote(string(out))
}

// latestCommit finder den nyeste commit for appens repo: først anonymt over https (offentlige repos), og
// ellers over SSH med appens deploy key, hvis den har en.
func (m *Manager) latestCommit(slug string, g GitSource) (string, error) {
	sha, err := m.remoteHead(g.Repo, g.Branch, "")
	if err == nil {
		return sha, nil
	}
	repo, perr := parseRepo(g.Repo)
	if perr != nil {
		return "", perr
	}
	var hasKey bool
	m.st.View(func(db *DB) { hasKey = db.GitKeys[slug] != nil && db.GitKeys[slug].Repo == repo.HTTPS() })
	if _, serr := os.Stat(m.keyPath(slug)); !hasKey || serr != nil {
		return "", errors.New("repoet kan ikke læses uden en deploy key: " + err.Error())
	}
	return m.remoteHead(repo.SSH(), g.Branch, m.keyPath(slug))
}

// CheckGit spørger git om den nyeste commit for appen og gemmer svaret (GitSource.Latest/Checked/CheckErr).
func (m *Manager) CheckGit(slug string) (string, error) {
	a, ok := m.get(slug)
	if !ok || a.Git == nil {
		return "", errors.New("appen er ikke hentet fra git")
	}
	sha, err := m.latestCommit(slug, *a.Git)
	m.update(slug, func(a *App) {
		if a.Git == nil {
			return
		}
		a.Git.Checked = time.Now()
		if err != nil {
			a.Git.CheckErr = err.Error()
			return
		}
		a.Git.Latest, a.Git.CheckErr = sha, ""
	})
	return sha, err
}

// currentCommit er den commit, appens kørende version er bygget fra (tom, hvis den ikke kendes).
func currentCommit(a App) string {
	for _, v := range a.Versions {
		if v.N == a.Current {
			return v.Commit
		}
	}
	return ""
}
