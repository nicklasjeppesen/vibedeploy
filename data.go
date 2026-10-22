package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxUpload     = 200 << 20 // pakket størrelse, der må sendes
	maxUnpacked   = 500 << 20 // udpakket størrelse
	maxFiles      = 20000
	keepVersions  = 5  // versioner med filer og image, der beholdes til rollback
	keepBackups   = 10 // backups pr. app
	maxLogSize    = 2 << 20
	backupPattern = `^\d{8}-\d{6}\.tar\.gz$`
)

var backupRe = regexp.MustCompile(backupPattern)

// extractTarGz pakker en tar.gz ud i dir med grænser og afviser stier, der prøver at komme ud af mappen.
// Symlinks og andre specialfiler springes over.
func extractTarGz(r io.Reader, dir string, maxBytes int64, maxCount int) (count int, skipped []string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, nil, errors.New("pakken er ikke en gyldig tar.gz")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	root := filepath.Clean(dir) + string(filepath.Separator)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if strings.Contains(err.Error(), "request body too large") {
				return count, skipped, errTooLarge
			}
			return count, skipped, fmt.Errorf("pakken kunne ikke læses: %v", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		if err := checkPath(name); err != nil {
			return count, skipped, fmt.Errorf("ugyldig sti i pakken: %q", h.Name)
		}
		base := path.Base(name)
		if strings.HasPrefix(base, "._") || base == ".DS_Store" { // macOS-rester
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(full, root) {
			return count, skipped, fmt.Errorf("ugyldig sti i pakken: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return count, skipped, err
			}
		case tar.TypeReg:
			count++
			if count > maxCount {
				return count, skipped, fmt.Errorf("pakken indeholder for mange filer (maks %d)", maxCount)
			}
			total += h.Size
			if total > maxBytes {
				return count, skipped, fmt.Errorf("pakken fylder for meget udpakket (maks %d MB)", maxBytes>>20)
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return count, skipped, err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 { // bevar kørselsret, fx på start-scripts
				mode = 0o755
			}
			f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return count, skipped, err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			f.Close()
			if err != nil {
				if strings.Contains(err.Error(), "request body too large") {
					return count, skipped, errTooLarge
				}
				return count, skipped, fmt.Errorf("pakken kunne ikke læses: %v", err)
			}
		case tar.TypeSymlink, tar.TypeLink:
			skipped = append(skipped, name)
		default:
			// pax-hoveder, enheder osv. ignoreres
		}
	}
	if count == 0 {
		return 0, skipped, errors.New("pakken er tom")
	}
	return count, skipped, nil
}

// errTooLarge returneres, når uploaden overskrider maxUpload.
var errTooLarge = fmt.Errorf("pakken er for stor (maks %d MB pakket)", maxUpload>>20)

// ---------- persistent data pr. app ----------

// volumeDir er appens datamappe på værten (monteres på /data i containeren).
func (m *Manager) volumeDir(slug string) string { return filepath.Join(m.dataDir, "volumes", slug) }

// backupDir er mappen med appens backups.
func (m *Manager) backupDir(slug string) string { return filepath.Join(m.dataDir, "backups", slug) }

// ensureVolume opretter appens /data-mappe. Den er åben for skrivning, fordi containere ofte kører
// som en anden bruger end serveren; forældremappen (data-mappen) er lukket (0700) for andre på maskinen.
func (m *Manager) ensureVolume(slug string) (string, error) {
	d := m.volumeDir(slug)
	if err := os.MkdirAll(d, 0o777); err != nil {
		return "", err
	}
	_ = os.Chmod(d, 0o777)
	return d, nil
}

// BackupInfo beskriver en backup til visning under fanen Data.
type BackupInfo struct {
	Name string
	Size string
	Ago  string
	Time time.Time
}

// Backup pakker appens /data-mappe. Tom mappe giver ingen backup.
func (m *Manager) Backup(slug, reason string) (string, error) {
	src := m.volumeDir(slug)
	if empty, _ := dirEmpty(src); empty {
		return "", nil
	}
	if err := os.MkdirAll(m.backupDir(slug), 0o700); err != nil {
		return "", err
	}
	name := time.Now().Format("20060102-150405") + ".tar.gz"
	dst := filepath.Join(m.backupDir(slug), name)
	f, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, in)
			in.Close()
			return err
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	f.Close()
	if err != nil {
		os.Remove(dst + ".tmp")
		return "", err
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		return "", err
	}
	m.logger(slug)("Backup af data taget (%s): %s", reason, name)
	m.pruneBackups(slug)
	return name, nil
}

// dirEmpty er sand, hvis mappen er tom eller ikke findes.
func dirEmpty(d string) (bool, error) {
	f, err := os.Open(d)
	if err != nil {
		return true, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err == io.EOF, nil
}

// Backups lister appens backups, nyeste først.
func (m *Manager) Backups(slug string) []BackupInfo {
	entries, _ := os.ReadDir(m.backupDir(slug))
	var out []BackupInfo
	for _, e := range entries {
		if !backupRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		t, _ := time.ParseInLocation("20060102-150405", strings.TrimSuffix(e.Name(), ".tar.gz"), time.Local)
		out = append(out, BackupInfo{Name: e.Name(), Size: humanSize(info.Size()), Ago: relTime(t), Time: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// pruneBackups sletter alt ud over de keepBackups nyeste backups.
func (m *Manager) pruneBackups(slug string) {
	b := m.Backups(slug)
	for i := keepBackups; i < len(b); i++ {
		_ = os.Remove(filepath.Join(m.backupDir(slug), b[i].Name))
	}
}

// Restore lægger en backup tilbage. Den nuværende data gemmes først som en ny backup.
func (m *Manager) Restore(slug, name string) error {
	if !backupRe.MatchString(name) {
		return errors.New("ukendt backup")
	}
	src := filepath.Join(m.backupDir(slug), name)
	f, err := os.Open(src)
	if err != nil {
		return errors.New("backuppen findes ikke")
	}
	defer f.Close()
	a, ok := m.get(slug)
	if !ok {
		return errors.New("appen findes ikke")
	}
	l := m.lockFor(slug)
	l.Lock()
	defer l.Unlock()
	m.stopAll(slug) // data må ikke ændres under appen, så her er der en kort pause
	if _, err := m.Backup(slug, "før gendannelse"); err != nil {
		return fmt.Errorf("kunne ikke tage backup af nuværende data: %v", err)
	}
	vol := m.volumeDir(slug)
	old := vol + ".old"
	_ = os.RemoveAll(old)
	_ = os.Rename(vol, old)
	if _, err := m.ensureVolume(slug); err != nil {
		return err
	}
	if _, _, err := extractTarGz(f, vol, 1<<40, 1<<30); err != nil && err.Error() != "pakken er tom" {
		_ = os.RemoveAll(vol)
		_ = os.Rename(old, vol)
		return fmt.Errorf("gendannelsen fejlede, den gamle data er lagt tilbage: %v", err)
	}
	_ = os.RemoveAll(old)
	m.logger(slug)("Data gendannet fra backup %s", name)
	if a.Current > 0 {
		img := a.Versions[a.Current-1].Image
		m.setStatus(slug, StatusStarting, "Starter din app …")
		go func() {
			l.Lock()
			defer l.Unlock()
			if err := m.startVersion(slug, a.Current, img); err != nil {
				m.setStatus(slug, StatusFailed, explain(err.Error()))
				return
			}
			m.setStatus(slug, StatusLive, "")
		}()
	}
	return nil
}

// DailyBackups tager backup af serverens database og af alle apps med data én gang i døgnet.
func (m *Manager) DailyBackups() {
	for range time.Tick(24 * time.Hour) {
		if err := m.BackupState(); err != nil {
			fmt.Println("Backup af state.db fejlede:", err)
		}
		for slug := range m.st.Snapshot().Apps {
			if _, err := m.Backup(slug, "dagligt"); err != nil {
				m.logger(slug)("Daglig backup fejlede: %v", err)
			}
		}
	}
}

// BackupState gemmer en kopi af state.db i backups/_server (de 7 nyeste beholdes).
// Mappenavnet kan ikke forveksles med en app, da slugs ikke må indeholde «_».
func (m *Manager) BackupState() error {
	dir := filepath.Join(m.dataDir, "backups", "_server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := m.st.BackupTo(filepath.Join(dir, "state-"+time.Now().Format("20060102-150405")+".db")); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(dir, "state-*.db"))
	sort.Strings(old)
	for i := 0; i < len(old)-7; i++ {
		_ = os.Remove(old[i])
	}
	return nil
}

// pruneVersions fjerner filer og images fra gamle versioner, så disken ikke fyldes.
func (m *Manager) pruneVersions(slug string) {
	a, ok := m.get(slug)
	if !ok {
		return
	}
	var drop []Version
	for i, v := range a.Versions {
		if v.Pruned || v.N == a.Current || i >= len(a.Versions)-keepVersions {
			continue
		}
		drop = append(drop, v)
	}
	for _, v := range drop {
		_ = os.RemoveAll(v.Dir)
		if v.Image != "" {
			m.rt.RemoveImage(v.Image)
		}
	}
	if len(drop) > 0 {
		m.update(slug, func(a *App) {
			for _, v := range drop {
				a.Versions[v.N-1].Pruned = true
				a.Versions[v.N-1].Image = ""
			}
		})
		m.logger(slug)("Ryddede %d gamle versioner op", len(drop))
	}
}

// humanSize viser en filstørrelse som B, KB eller MB.
func humanSize(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
