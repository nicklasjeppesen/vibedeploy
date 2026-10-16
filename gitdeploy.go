package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	shortRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	hostRe      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	repoPathRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)
	branchRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	scpLikeRe   = regexp.MustCompile(`^git@([^:/]+):(.+)$`)
)

// GitRepo er et normaliseret repo med både https- og ssh-adresse.
type GitRepo struct {
	Host, Path string // fx github.com og nicklas/blog
}

// HTTPS er repoets adresse til hentning uden nøgle (offentlige repos).
func (g GitRepo) HTTPS() string { return "https://" + g.Host + "/" + g.Path + ".git" }

// SSH er repoets adresse til hentning med deploy key (private repos).
func (g GitRepo) SSH() string { return "git@" + g.Host + ":" + g.Path + ".git" }

// AddKeyURL er siden, hvor man tilføjer en deploy key (kun kendt for GitHub og GitLab).
func (g GitRepo) AddKeyURL() string {
	switch g.Host {
	case "github.com":
		return "https://github.com/" + g.Path + "/settings/keys/new"
	case "gitlab.com":
		return "https://gitlab.com/" + g.Path + "/-/settings/repository"
	}
	return ""
}

// parseRepo accepterer «ejer/repo» (GitHub), https://vært/sti, git@vært:sti og ssh://git@vært/sti.
// Alt andet afvises, så git aldrig kan bruges til at læse lokale filer eller andre protokoller.
func parseRepo(in string) (GitRepo, error) {
	in = strings.TrimSpace(in)
	bad := fmt.Errorf("«%s» er ikke en repo-adresse, jeg kan bruge. Brug fx ejer/repo eller https://github.com/ejer/repo", in)
	var host, path string
	switch {
	case shortRepoRe.MatchString(in):
		host, path = "github.com", in
	case strings.HasPrefix(in, "https://"), strings.HasPrefix(in, "ssh://"):
		u, err := url.Parse(in)
		if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
			return GitRepo{}, bad
		}
		if u.User != nil && (u.Scheme == "https" || u.User.Username() != "git") {
			return GitRepo{}, bad
		}
		host, path = strings.ToLower(u.Hostname()), strings.Trim(u.Path, "/")
	default:
		m := scpLikeRe.FindStringSubmatch(in)
		if m == nil {
			return GitRepo{}, bad
		}
		host, path = strings.ToLower(m[1]), strings.Trim(m[2], "/")
	}
	path = strings.TrimSuffix(path, ".git")
	if !hostRe.MatchString(host) || !strings.Contains(host, ".") || host == "localhost" ||
		!repoPathRe.MatchString(path) || strings.Contains(path, "..") || strings.HasPrefix(path, "-") {
		return GitRepo{}, bad
	}
	return GitRepo{Host: host, Path: path}, nil
}

// checkBranch afviser grennavne, der kunne læses som git-flag eller indeholder mærkelige tegn.
// En tom gren betyder repoets standardgren.
func checkBranch(b string) error {
	if b == "" {
		return nil
	}
	if !branchRe.MatchString(b) || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return fmt.Errorf("«%s» er ikke et gyldigt grennavn", b)
	}
	return nil
}

// ErrNeedKey betyder, at repoet ikke kunne hentes uden en deploy key.
type ErrNeedKey struct {
	Public, AddURL, Repo string
	NewKey               bool
}

// Error gør ErrNeedKey til en error.
func (e *ErrNeedKey) Error() string { return "repoet kræver en deploy key" }

// keyPath er stien til appens private deploy key (den forlader aldrig serveren).
func (m *Manager) keyPath(slug string) string {
	return filepath.Join(m.dataDir, "keys", slug+"_ed25519")
}

// ensureKey finder eller laver appens deploy key. En nøgle er bundet til ejer og repo.
func (m *Manager) ensureKey(slug, ownerID string, repo GitRepo) (pub string, created bool, err error) {
	var existing *GitKey
	m.st.View(func(db *DB) {
		if k := db.GitKeys[slug]; k != nil {
			cp := *k
			existing = &cp
		}
	})
	if existing != nil {
		if existing.OwnerID != ownerID {
			return "", false, fmt.Errorf("navnet «%s» er optaget; vælg et andet navn", slug)
		}
		if existing.Repo == repo.HTTPS() {
			if _, err := os.Stat(m.keyPath(slug)); err == nil {
				return existing.Public, false, nil
			}
		}
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return "", false, errors.New("ssh-keygen findes ikke på serveren (installér openssh-client)")
	}
	p := m.keyPath(slug)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", false, err
	}
	_ = os.Remove(p)
	_ = os.Remove(p + ".pub")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "vibedeploy-"+slug, "-f", p).CombinedOutput(); err != nil {
		return "", false, fmt.Errorf("kunne ikke lave nøgle: %s", strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(p + ".pub")
	if err != nil {
		return "", false, err
	}
	pub = strings.TrimSpace(string(b))
	err = m.st.Do(func(db *DB) error {
		db.GitKeys[slug] = &GitKey{OwnerID: ownerID, Repo: repo.HTTPS(), Public: pub, Created: time.Now()}
		return nil
	})
	return pub, true, err
}

// RemoveKey sletter appens deploy key, så næste hentning laver en ny.
func (m *Manager) RemoveKey(slug string) {
	_ = os.Remove(m.keyPath(slug))
	_ = os.Remove(m.keyPath(slug) + ".pub")
	_ = m.st.Do(func(db *DB) error { delete(db.GitKeys, slug); return nil })
}

// shQuote sætter s i enkelte anførselstegn til brug i en shell-kommando (GIT_SSH_COMMAND).
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// gitClone laver en flad kloning (kun nyeste commit) af repoURL ind i dir. Kun https og ssh er
// tilladt, git spørger aldrig om adgangskode, og key bruges som SSH-nøgle, hvis den er sat.
// Gits output returneres til fejlbeskeder.
func (m *Manager) gitClone(repoURL, branch, dir, key string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git er ikke installeret på serveren (apt install git)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	args := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always",
		"clone", "--depth", "1", "--single-branch", "--no-tags"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", repoURL, dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/true", "GIT_CONFIG_NOSYSTEM=1")
	if key != "" {
		known := filepath.Join(m.dataDir, "keys", "known_hosts")
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+shQuote(key)+" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=15"+
			" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="+shQuote(known))
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// DeployFromGit henter appen fra et repo. Offentlige repos hentes direkte; private kræver appens deploy key.
func (m *Manager) DeployFromGit(name, repoIn, branch string, port int, note, ownerID string) (*DeployResult, error) {
	repo, err := parseRepo(repoIn)
	if err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	slug := slugify(name)
	if !slugRe.MatchString(slug) || reserved[slug] {
		return nil, fmt.Errorf("navnet «%s» kan ikke bruges som adresse; vælg et andet", name)
	}
	var needKey *ErrNeedKey
	how := "git: " + repo.Host + "/" + repo.Path
	if branch != "" {
		how += " (" + branch + ")"
	}
	res, err := m.deployWith(name, port, note, ownerID, how, "", func(dir string) error {
		// 1) prøv uden nøgle (offentligt repo)
		out, err := m.gitClone(repo.HTTPS(), branch, dir, "")
		if err == nil {
			return cleanClone(dir)
		}
		if strings.Contains(out, "Remote branch") || strings.Contains(out, "not found in upstream") {
			return fmt.Errorf("grenen «%s» findes ikke i repoet", branch)
		}
		_ = os.RemoveAll(dir)
		_ = os.MkdirAll(dir, 0o755)
		// 2) privat: brug appens deploy key
		pub, created, kerr := m.ensureKey(slug, ownerID, repo)
		if kerr != nil {
			return kerr
		}
		if created {
			needKey = &ErrNeedKey{Public: pub, AddURL: repo.AddKeyURL(), Repo: repo.Host + "/" + repo.Path, NewKey: true}
			return needKey
		}
		out, err = m.gitClone(repo.SSH(), branch, dir, m.keyPath(slug))
		if err != nil {
			m.logger(slug)("git-hentning fejlede: %s", lastLines(out, 3))
			if strings.Contains(out, "Remote branch") || strings.Contains(out, "not found in upstream") {
				return fmt.Errorf("grenen «%s» findes ikke i repoet", branch)
			}
			needKey = &ErrNeedKey{Public: pub, AddURL: repo.AddKeyURL(), Repo: repo.Host + "/" + repo.Path}
			return needKey
		}
		return cleanClone(dir)
	})
	if needKey != nil {
		return nil, needKey
	}
	if err != nil {
		return nil, err
	}
	m.update(res.App.Slug, func(a *App) { a.Git = &GitSource{Repo: repo.HTTPS(), Branch: branch} })
	return res, nil
}

// cleanClone fjerner .git og symlinks og håndhæver de samme grænser som upload.
func cleanClone(dir string) error {
	_ = os.RemoveAll(filepath.Join(dir, ".git"))
	var total int64
	count := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return os.Remove(p)
		}
		if info.Mode().IsRegular() {
			count++
			total += info.Size()
			if count > maxFiles || total > maxUnpacked {
				return fmt.Errorf("repoet er for stort (maks %d filer og %d MB)", maxFiles, maxUnpacked>>20)
			}
		}
		return nil
	})
	if err == nil && count == 0 {
		err = errors.New("repoet er tomt")
	}
	return err
}

// lastLines returnerer de sidste n linjer af s samlet på én linje.
func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " / ")
}

// needKeyText er beskeden til AI'en og ejeren, når et repo kræver en deploy key:
// hvor nøglen skal tilføjes, og selve den offentlige nøgle.
func needKeyText(e *ErrNeedKey) string {
	where := "i repoets indstillinger under «Deploy keys»"
	if e.AddURL != "" {
		where = "her: " + e.AddURL
	}
	head := "Repoet " + e.Repo + " er privat (eller findes ikke), så serveren skal have lov til at læse det."
	if !e.NewKey {
		head = "Serveren kunne stadig ikke hente " + e.Repo + ". Er nøglen tilføjet til det rigtige repo, og findes repoet?"
	}
	return head + "\n\nTilføj denne nøgle som deploy key " + where +
		"\nGiv den et navn, fx «Vibe Deploy», og lad «Allow write access» være slået FRA (serveren skal kun læse).\n\n" +
		e.Public + "\n\nKør derefter deploy_from_git igen. Nøglen kan også ses under appens Indstillinger i dashboardet."
}
