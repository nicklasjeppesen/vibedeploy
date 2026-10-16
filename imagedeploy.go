package main

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Login til et privat registry ligger som to hemmeligheder på appen. De bruges kun af serveren til
// at hente imaget og sendes aldrig ind i containeren (se envFor).
const (
	RegistryUserKey  = "DOCKER_BRUGERNAVN"
	RegistryTokenKey = "DOCKER_TOKEN"
)

var registryKeys = []string{RegistryUserKey, RegistryTokenKey}

var (
	imagePartRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	imageTagRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// errRegistryAuth betyder, at registryet afviste hentningen: imaget er privat (og login mangler
// eller er forkert), eller det findes ikke.
var errRegistryAuth = errors.New("imaget findes ikke, eller det er privat og kræver login")

// ImageRef er en normaliseret henvisning til et færdigt image, fx nginx:1.27 eller ghcr.io/ejer/app:v2.
type ImageRef struct {
	Host, Path, Tag, Digest string // Host er tom for Docker Hub
}

// String er referencen, som docker pull forstår den.
func (r ImageRef) String() string {
	s := r.Path
	if r.Host != "" {
		s = r.Host + "/" + s
	}
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// DockerHub fortæller, om imaget ligger på Docker Hub.
func (r ImageRef) DockerHub() bool { return r.Host == "" }

// authKey er nøglen i Dockers config.json, som login til registryet gemmes under.
func (r ImageRef) authKey() string {
	if r.DockerHub() {
		return "https://index.docker.io/v1/"
	}
	return r.Host
}

// parseImage accepterer «navn», «ejer/navn», «vært/ejer/navn» med valgfrit :tag og @sha256:….
// Uden tag bruges latest. Værter skal have et punktum og må ikke have port, være en IP eller localhost,
// så serveren ikke kan bruges til at kalde interne adresser.
func parseImage(in string) (ImageRef, error) {
	in = strings.TrimSpace(in)
	bad := fmt.Errorf("«%s» er ikke et image-navn, jeg kan bruge. Brug fx nginx:1.27 eller ejer/app:latest (små bogstaver)", in)
	if in == "" || len(in) > 255 || strings.HasPrefix(in, "-") {
		return ImageRef{}, bad
	}
	var r ImageRef
	if i := strings.Index(in, "@"); i >= 0 {
		r.Digest, in = in[i+1:], in[:i]
		if !digestRe.MatchString(r.Digest) {
			return ImageRef{}, bad
		}
	}
	if i := strings.LastIndex(in, ":"); i > strings.LastIndex(in, "/") {
		r.Tag, in = in[i+1:], in[:i]
		if !imageTagRe.MatchString(r.Tag) {
			return ImageRef{}, bad
		}
	}
	parts := strings.Split(in, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		host := strings.ToLower(parts[0])
		if !hostRe.MatchString(host) || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
			return ImageRef{}, bad
		}
		if host != "docker.io" && host != "index.docker.io" {
			r.Host = host
		}
		parts = parts[1:]
	}
	for _, p := range parts {
		if !imagePartRe.MatchString(p) {
			return ImageRef{}, bad
		}
	}
	r.Path = strings.Join(parts, "/")
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// isRegistryKey fortæller, om en hemmelighed er login til registryet (og ikke til appen).
func isRegistryKey(k string) bool { return k == RegistryUserKey || k == RegistryTokenKey }

// registryLogin henter appens udfyldte login til registryet (tomt, hvis det ikke er sat).
func registryLogin(a App) (user, token string) {
	for _, kv := range a.Secrets {
		switch kv.Key {
		case RegistryUserKey:
			user = kv.Value
		case RegistryTokenKey:
			token = kv.Value
		}
	}
	return user, token
}

// needsRegistryLogin er sand, når appen har pladser til et registry-login, som ikke er udfyldt.
func needsRegistryLogin(a App) bool {
	for _, kv := range a.Secrets {
		if isRegistryKey(kv.Key) && kv.Value == "" {
			return true
		}
	}
	return false
}

// ErrNeedLogin betyder, at imaget er privat, og at ejeren først skal indsætte login i dashboardet.
type ErrNeedLogin struct{ Ref ImageRef }

// Error gør ErrNeedLogin til en error.
func (e *ErrNeedLogin) Error() string { return "imaget er privat og kræver login" }

// DeployImage udgiver et færdigt image fra Docker Hub eller et andet registry uden at bygge noget.
// Er imaget privat, oprettes pladser til login, som ejeren udfylder i dashboardet. En ny app venter
// alligevel på godkendelse, så den oprettes med det samme; en godkendt app hentes først, når login er på plads.
func (m *Manager) DeployImage(name, refIn string, port int, private bool, note, ownerID string) (*DeployResult, error) {
	ref, err := parseImage(refIn)
	if err != nil {
		return nil, err
	}
	slug := slugify(name)
	if existing, ok := m.get(slug); ok && existing.Approved && private {
		if err := m.RequestSecrets(slug, registryKeys); err != nil {
			return nil, err
		}
		if existing, _ = m.get(slug); needsRegistryLogin(existing) {
			return nil, &ErrNeedLogin{Ref: ref}
		}
	}
	res, err := m.deployWith(name, port, note, ownerID, "image: "+ref.String(), ref.String(), func(dir string) error { return nil })
	if err != nil {
		return nil, err
	}
	if private {
		if err := m.RequestSecrets(res.App.Slug, registryKeys); err != nil {
			return nil, err
		}
		res.App, _ = m.get(res.App.Slug)
	}
	return res, nil
}

// registryLoginText forklarer ejeren, hvor login til et privat image skal indsættes.
func registryLoginText(ref ImageRef, settingsURL string) string {
	how := "brugernavnet og en adgangsnøgle med kun læseadgang til registryet " + ref.Host
	switch {
	case ref.DockerHub():
		how = "dit Docker Hub-brugernavn og en access token med kun læseadgang (laves på https://app.docker.com/settings/personal-access-tokens)"
	case ref.Host == "ghcr.io":
		how = "dit GitHub-brugernavn og en personal access token (classic) med kun «read:packages» (laves på https://github.com/settings/tokens/new?scopes=read:packages)"
	}
	return fmt.Sprintf("Imaget %s er privat, så serveren skal have lov til at hente det. Bed ejeren åbne %s og indsætte %s i %s og %s. Login bruges kun til at hente imaget og sendes ikke ind i appen. Det må ikke skrives i chatten.",
		ref, settingsURL, how, RegistryUserKey, RegistryTokenKey)
}
