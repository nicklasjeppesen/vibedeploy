package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ---------- Oprydning af Docker-cache (siden Server) ----------
//
// Hver bygning efterlader en byggecache (lag fra base-images, hentede pakker …), og over tid fylder den
// ofte flere GB. Ejeren kan rydde op med én knap. Der fjernes kun ting, der kan laves igen:
//
//  1. Byggecachen («docker builder prune -af»). Den næste bygning af hver app tager lidt længere tid,
//     fordi base-images og pakker hentes igen.
//  2. Gamle images fra Vibe Deploy (vd-<slug>:v<N>), som ingen app bruger: fra slettede apps og versioner,
//     der er ryddet op. Images til versioner, man kan rulle tilbage til, og til appens kørende version røres
//     ikke, og heller ikke images til en app, der er ved at bygge eller starte.
//  3. Lag uden navn («dangling», «docker image prune -f»), som er tilovers fra tidligere bygninger.
//
// Andre images og containere på serveren røres ikke, og «docker rmi» kører uden -f, så et image, der er i
// brug, aldrig fjernes.

// cleanState er status for den seneste oprydning.
type cleanState struct {
	Running           bool
	Started, Finished time.Time
	By                string
	Freed             int64    // bytes, Docker oplyste som frigjort (images regnet med deres fulde størrelse)
	Done              []string // hvad der blev gjort, i ord
	Err               string
}

// dockerImage er én linje fra «docker images».
type dockerImage struct {
	Ref, ID string // Ref er «navn:tag» (eller «<none>:<none>» for lag uden navn)
	Size    int64
}

// parseDockerImages læser «docker images --format '{{.Repository}}:{{.Tag}}\t{{.ID}}\t{{.Size}}'».
func parseDockerImages(out string) []dockerImage {
	var res []dockerImage
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		res = append(res, dockerImage{Ref: strings.TrimSpace(f[0]), ID: strings.TrimSpace(f[1]), Size: int64(parseSize(f[2]))})
	}
	return res
}

// keepImages er de images, Vibe Deploy skal beholde: alle byggede versioner, der ikke er ryddet op
// (den kørende og dem, man kan rulle tilbage til), og alt fra apps, der bygger eller starter lige nu
// (busy), fordi deres nye image først gemmes på versionen, når bygningen er færdig.
func keepImages(db DB) (keep map[string]bool, busy map[string]bool) {
	keep, busy = map[string]bool{}, map[string]bool{}
	for slug, a := range db.Apps {
		if a.Status == StatusBuilding || a.Status == StatusStarting {
			busy[slug] = true
		}
		for _, v := range a.Versions {
			if v.Image != "" && !v.Pruned {
				keep[v.Image] = true
			}
		}
	}
	return keep, busy
}

// orphanImages er Vibe Deploys egne images (vd-<slug>:…), som ingen app bruger og ingen container kører.
func orphanImages(imgs []dockerImage, keep, busy, inUse map[string]bool) []dockerImage {
	var out []dockerImage
	for _, im := range imgs {
		repo, _, _ := strings.Cut(im.Ref, ":")
		if !strings.HasPrefix(repo, "vd-") || keep[im.Ref] || inUse[im.Ref] || inUse[im.ID] {
			continue
		}
		if busy[strings.TrimPrefix(repo, "vd-")] {
			continue
		}
		out = append(out, im)
	}
	return out
}

// parseReclaimed finder, hvor meget Docker frigjorde, i output fra «docker builder prune» og
// «docker image prune» («Total reclaimed space: 1.2GB» eller «Total:	1.2GB»).
func parseReclaimed(out string) int64 {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		for _, pre := range []string{"Total reclaimed space:", "Total:"} {
			if v, ok := strings.CutPrefix(line, pre); ok {
				return int64(parseSize(v))
			}
		}
	}
	return 0
}

// Images henter alle images på serveren.
func (d *dockerRT) Images(ctx context.Context) ([]dockerImage, error) {
	out, err := exec.CommandContext(ctx, "docker", "images", "--format", "{{.Repository}}:{{.Tag}}\t{{.ID}}\t{{.Size}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker images fejlede: %w", err)
	}
	return parseDockerImages(string(out)), nil
}

// ContainerImages er de images, som en container bruger, også stoppede containere. Docker viser navnet
// («vd-blog:v3») eller, hvis navnet er fjernet, de første 12 tegn af id'et, som «docker images» også gør.
func (d *dockerRT) ContainerImages(ctx context.Context) (map[string]bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.Image}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps fejlede: %w", err)
	}
	return parseContainerImages(string(out)), nil
}

// parseContainerImages læser én image pr. linje. Et image uden tag hedder også «:latest» hos «docker images».
func parseContainerImages(out string) map[string]bool {
	used := map[string]bool{}
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			used[f] = true
			if !strings.Contains(f, ":") {
				used[f+":latest"] = true
			}
		}
	}
	return used
}

// cacheView er det, siden Server viser om Docker-cachen.
type cacheView struct {
	Docker                bool
	BuildCache, Reclaim   string // byggecachens størrelse og hvor meget, der kan frigøres
	Orphans               int
	OrphanSize            string
	Running               bool
	Last, LastBy, LastErr string
	LastFreed             string
	LastDone              []string
}

// cacheInfo samler siden Servers oplysninger om Docker-cachen: byggecachen fra «docker system df» (målt af
// Monitor) og de gamle images, der kan fjernes (slås op nu, højst et par sekunder).
func (s *Server) cacheInfo(df []DockerDF) cacheView {
	v := cacheView{}
	s.cleanMu.Lock()
	st := s.clean
	s.cleanMu.Unlock()
	v.Running = st.Running
	if !st.Finished.IsZero() {
		v.Last, v.LastBy, v.LastErr, v.LastDone = relTime(st.Finished), st.By, st.Err, st.Done
		v.LastFreed = humanSize(st.Freed)
	}
	d, ok := s.rt.(*dockerRT)
	if !ok {
		return v
	}
	v.Docker = true
	v.BuildCache, v.Reclaim = "–", "–"
	for _, x := range df {
		if strings.EqualFold(x.Type, "Build Cache") {
			v.BuildCache = x.Size
			v.Reclaim, _, _ = strings.Cut(x.Reclaimable, " ") // «1.2GB (100%)» → «1.2GB»
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	imgs, err := d.Images(ctx)
	if err != nil {
		return v
	}
	inUse, err := d.ContainerImages(ctx)
	if err != nil {
		return v
	}
	keep, busy := keepImages(s.st.Snapshot())
	var size int64
	for _, im := range orphanImages(imgs, keep, busy, inUse) {
		v.Orphans++
		size += im.Size
	}
	v.OrphanSize = humanSize(size)
	return v
}

// startCleanup starter en oprydning i baggrunden. Den returnerer false, hvis en oprydning allerede kører.
func (s *Server) startCleanup(by string) bool {
	d, ok := s.rt.(*dockerRT)
	if !ok {
		return false
	}
	s.cleanMu.Lock()
	if s.clean.Running {
		s.cleanMu.Unlock()
		return false
	}
	s.clean = cleanState{Running: true, Started: time.Now(), By: by}
	s.cleanMu.Unlock()
	go func() {
		st := s.runCleanup(d)
		s.cleanMu.Lock()
		st.Started, st.By, st.Finished, st.Running = s.clean.Started, by, time.Now(), false
		s.clean = st
		s.cleanMu.Unlock()
		if s.mon != nil {
			s.mon.Remeasure()
		}
	}()
	return true
}

// runCleanup gør selve oprydningen (se toppen af filen) og fortæller, hvad der blev gjort.
func (s *Server) runCleanup(d *dockerRT) cleanState {
	var st cleanState
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	fmt.Println("Oprydning af Docker-cache startet")

	// 1. Byggecachen.
	out, err := exec.CommandContext(ctx, "docker", "builder", "prune", "-af").CombinedOutput()
	if err != nil {
		st.Err = "Byggecachen kunne ikke ryddes: " + firstLine(string(out), err)
	} else {
		n := parseReclaimed(string(out))
		st.Freed += n
		st.Done = append(st.Done, "Byggecachen er ryddet ("+humanSize(n)+").")
	}

	// 2. Gamle images fra Vibe Deploy, som ingen app bruger.
	imgs, err := d.Images(ctx)
	var inUse map[string]bool
	if err == nil {
		inUse, err = d.ContainerImages(ctx)
	}
	if err != nil {
		if st.Err == "" {
			st.Err = "Gamle images kunne ikke findes: " + err.Error()
		}
	} else {
		keep, busy := keepImages(s.st.Snapshot())
		removed, failed := 0, 0
		var size int64
		for _, im := range orphanImages(imgs, keep, busy, inUse) {
			if out, err := exec.CommandContext(ctx, "docker", "rmi", im.Ref).CombinedOutput(); err != nil {
				failed++
				fmt.Printf("Oprydning: %s blev ikke fjernet: %s\n", im.Ref, firstLine(string(out), err))
				continue
			}
			removed++
			size += im.Size
		}
		st.Freed += size
		switch {
		case removed > 0:
			st.Done = append(st.Done, fmt.Sprintf("%d gamle %s fjernet (op til %s).", removed, plural(removed, "image er", "images er"), humanSize(size)))
		case failed == 0:
			st.Done = append(st.Done, "Der var ingen gamle images at fjerne.")
		}
		if failed > 0 {
			st.Done = append(st.Done, fmt.Sprintf("%d %s blev ikke fjernet, fordi de stadig er i brug.", failed, plural(failed, "image", "images")))
		}
	}

	// 3. Lag uden navn.
	out, err = exec.CommandContext(ctx, "docker", "image", "prune", "-f").CombinedOutput()
	if err != nil {
		if st.Err == "" {
			st.Err = "Ubrugte lag kunne ikke ryddes: " + firstLine(string(out), err)
		}
	} else if n := parseReclaimed(string(out)); n > 0 {
		st.Freed += n
		st.Done = append(st.Done, "Ubrugte lag er ryddet ("+humanSize(n)+").")
	}
	fmt.Printf("Oprydning af Docker-cache færdig: %s frigjort\n", humanSize(st.Freed))
	return st
}

// firstLine er den første linje af kommandoens output, eller fejlen, hvis der intet output er.
func firstLine(out string, err error) string {
	if l, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); l != "" {
		return l
	}
	return err.Error()
}

// serverCleanup er knappen «Ryd op nu» på siden Server.
func (s *Server) serverCleanup(w http.ResponseWriter, r *http.Request, u *User) {
	if _, ok := s.rt.(*dockerRT); !ok {
		s.page(w, r, "message.html", "Oprydning", map[string]any{"Text": "Oprydning af cache findes kun, når Vibe Deploy kører med Docker.", "Back": "/server"})
		return
	}
	msg := "cleaning"
	if !s.startCleanup(u.Name) {
		msg = "cleanbusy"
	}
	http.Redirect(w, r, "/server?m="+msg+"#cache", http.StatusSeeOther)
}
