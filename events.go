package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- Live opdatering af appens side (Server-Sent Events) ----------
//
// Appens side i dashboardet åbner en EventSource mod /apps/<slug>/events. Serveren sender:
//   - «status», når appens status, tekst eller kørende version ændrer sig (JSON, se statusEvent).
//   - «log», for hver ny linje i bygge- og deployloggen (ren tekst).
// Det er envejs (server → browser), så standardbibliotekets http.Flusher er nok; der er ingen afhængighed.
// Browseren forbinder selv igen, hvis forbindelsen ryger (fx når Vibe Deploy opdateres).
//
// Manager.events er en lille «hub»: hver åben side har en kanal pr. app. Beskeder sendes til alle, der
// lytter på appen. En side, der ikke når at læse, springer beskeder over i stedet for at bremse bygningen;
// status tjekkes desuden hvert statusRecheck, så en overset ændring altid kommer frem.

// event er én besked til en side.
type event struct {
	kind, data string // kind: "status" eller "log"; ved status er data tom (siden læser selv appen)
	slug       string // appen, beskeden handler om (bruges af forsiden, der lytter på alle apps)
}

// allApps er nøglen i hub'en for forsiden, der får statusbeskeder for alle apps (ikke loglinjer).
const allApps = "*"

// hub fordeler beskeder til de sider, der lytter på en app.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan event]struct{}
}

// subscribe begynder at lytte på appen slug. Kald cancel, når siden lukkes.
func (h *hub) subscribe(slug string) (ch chan event, cancel func()) {
	ch = make(chan event, 64)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan event]struct{}{}
	}
	if h.subs[slug] == nil {
		h.subs[slug] = map[chan event]struct{}{}
	}
	h.subs[slug][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[slug], ch)
		if len(h.subs[slug]) == 0 {
			delete(h.subs, slug)
		}
		h.mu.Unlock()
	}
}

// publish sender e til alle, der lytter på slug. Den blokerer aldrig: er en kanal fuld, springes den over.
func (h *hub) publish(slug string, e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e.slug = slug
	send := func(subs map[chan event]struct{}) {
		for ch := range subs {
			select {
			case ch <- e:
			default:
			}
		}
	}
	send(h.subs[slug])
	if e.kind == "status" && slug != allApps {
		send(h.subs[allApps])
	}
}

// listeners er antallet af åbne sider for slug (bruges i tests).
func (h *hub) listeners(slug string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[slug])
}

// notify fortæller siderne for slug, at appen kan have ændret sig.
func (m *Manager) notify(slug string) { m.events.publish(slug, event{kind: "status"}) }

// statusEvent er det, siden får at vide om appens tilstand.
type statusEvent struct {
	Status  string `json:"status"`
	Label   string `json:"label"`
	Class   string `json:"class"`
	Text    string `json:"text"`
	Current int    `json:"current"`
	Busy    bool   `json:"busy"` // bygger eller starter
}

// statusOf laver statusEvent for en app.
func statusOf(a App) statusEvent {
	return statusEvent{Status: a.Status, Label: statusLabel(a.Status), Class: statusClass(a.Status), Text: a.StatusText,
		Current: a.Current, Busy: a.Status == StatusBuilding || a.Status == StatusStarting}
}

// Hvor tit forbindelsen holdes i live, og hvor tit status tjekkes, selv uden besked.
var (
	sseHeartbeat  = 25 * time.Second
	statusRecheck = 5 * time.Second
)

// appEvents er /apps/<slug>/events: en strøm af status og nye loglinjer for appen, så længe siden er åben.
func (s *Server) appEvents(w http.ResponseWriter, r *http.Request, u *User) {
	slug := r.PathValue("slug")
	a, ok := s.mgr.get(slug)
	if !ok || !canManage(&a, u) {
		http.NotFound(w, r)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming understøttes ikke", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.mgr.events.subscribe(slug)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // en proxy foran må ikke samle svaret op
	fmt.Fprint(w, "retry: 2000\n\n") // forbind igen efter 2 sekunder, hvis forbindelsen ryger

	var last statusEvent
	sendStatus := func(force bool) bool {
		a, ok := s.mgr.get(slug)
		if !ok {
			fmt.Fprint(w, "event: gone\ndata: {}\n\n") // appen er slettet
			fl.Flush()
			return false
		}
		st := statusOf(a)
		if st == last && !force {
			return true
		}
		last = st
		b, _ := json.Marshal(st)
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", b)
		fl.Flush()
		return true
	}
	if !sendStatus(true) {
		return
	}
	beat := time.NewTicker(sseHeartbeat)
	defer beat.Stop()
	recheck := time.NewTicker(statusRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing():
			return // Vibe Deploy lukker ned; browseren forbinder selv igen til den nye proces
		case e := <-ch:
			switch e.kind {
			case "log":
				fmt.Fprintf(w, "event: log\ndata: %s\n\n", sseLine(e.data))
				fl.Flush()
			default:
				if !sendStatus(false) {
					return
				}
			}
		case <-recheck.C:
			if !sendStatus(false) {
				return
			}
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// homeFingerprint er en kort beskrivelse af det, forsiden viser for brugeren u: hver synlig app med status,
// tekst og version. Ændrer den sig, skal forsiden tegnes igen.
func (s *Server) homeFingerprint(u *User) string {
	db := s.st.Snapshot()
	var parts []string
	for slug, a := range db.Apps {
		if canOpen(a, u) {
			parts = append(parts, fmt.Sprintf("%s|%s|%s|%d|%s", slug, a.Status, a.StatusText, a.Current, a.Visibility))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// homeEvents er /events: forsiden får «changed», når noget, den viser, har ændret sig. Siden henter så selv
// sit indhold igen og skifter det ud uden at genindlæse (app.js). Beskeden indeholder ingen oplysninger om
// apps, så den kan ikke afsløre andre brugeres apps.
func (s *Server) homeEvents(w http.ResponseWriter, r *http.Request, u *User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming understøttes ikke", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.mgr.events.subscribe(allApps)
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()

	last := s.homeFingerprint(u)
	check := func() {
		if fp := s.homeFingerprint(u); fp != last {
			last = fp
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
			fl.Flush()
		}
	}
	beat := time.NewTicker(sseHeartbeat)
	defer beat.Stop()
	recheck := time.NewTicker(statusRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing():
			return
		case <-ch:
			check()
		case <-recheck.C:
			check()
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// sseLine gør en logline sikker til ét data-felt (linjeskift ville afslutte beskeden).
func sseLine(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c == '\n' || c == '\r' {
			b[i] = ' '
		}
	}
	return string(b)
}

// closing er en kanal, der lukkes, når serveren begynder at lukke ned (shutdown i main), så åbne
// strømme slutter med det samme. Ellers ville nedlukningen vente på dem og gøre en opdatering langsommere.
func (s *Server) closing() <-chan struct{} {
	s.closeOnce.Do(func() { s.closeCh = make(chan struct{}) })
	return s.closeCh
}

// beginShutdown lukker closing-kanalen (højst én gang).
func (s *Server) beginShutdown() {
	s.closing()
	s.shutOnce.Do(func() { close(s.closeCh) })
}
