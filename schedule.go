package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // tidszonerne er bygget ind, så tidspunktet virker på servere uden tzdata
)

// ---------- Tidspunkt for automatiske opdateringer af Vibe Deploy ----------
//
// Ejeren vælger en dag (hver dag eller en bestemt ugedag), et klokkeslæt og en tidszone (standard
// Europe/Copenhagen). Tidspunktet regnes i den tidszone, uanset hvad serverens ur står på (ofte UTC), og
// følger dens sommertid. Opdateringen må gå i gang fra det valgte
// tidspunkt og i updateWindow derefter, så en server, der var nede præcis på minuttet, stadig når det.
// Der prøves højst én gang pr. tidspunkt (Settings.LastAutoTry).

// defaultUpdateTZ er tidszonen, når ejeren ikke har valgt en anden.
const defaultUpdateTZ = "Europe/Copenhagen"

// updateTZs er valgene i dropdown'en Tidszone, med danske navne. Ejeren kan også vælge «Andet …» og
// skrive enhver anden IANA-tidszone.
var updateTZs = []struct{ Zone, Label string }{
	{"Europe/Copenhagen", "Danmark (København)"},
	{"Atlantic/Faroe", "Færøerne (Tórshavn)"},
	{"America/Nuuk", "Grønland (Nuuk)"},
	{"Europe/Oslo", "Norge (Oslo)"},
	{"Europe/Stockholm", "Sverige (Stockholm)"},
	{"Europe/Helsinki", "Finland (Helsinki)"},
	{"Atlantic/Reykjavik", "Island (Reykjavík)"},
	{"Europe/Berlin", "Tyskland (Berlin)"},
	{"Europe/Amsterdam", "Holland (Amsterdam)"},
	{"Europe/Paris", "Frankrig (Paris)"},
	{"Europe/Madrid", "Spanien (Madrid)"},
	{"Europe/Rome", "Italien (Rom)"},
	{"Europe/Warsaw", "Polen (Warszawa)"},
	{"Europe/London", "Storbritannien (London)"},
	{"Europe/Dublin", "Irland (Dublin)"},
	{"America/New_York", "USA, østkyst (New York)"},
	{"America/Chicago", "USA, midten (Chicago)"},
	{"America/Denver", "USA, bjergstaterne (Denver)"},
	{"America/Los_Angeles", "USA, vestkyst (Los Angeles)"},
	{"Asia/Dubai", "Forenede Arabiske Emirater (Dubai)"},
	{"Asia/Kolkata", "Indien (Kolkata)"},
	{"Asia/Singapore", "Singapore"},
	{"Asia/Tokyo", "Japan (Tokyo)"},
	{"Australia/Sydney", "Australien (Sydney)"},
	{"UTC", "UTC (verdenstid)"},
}

// tzOther er værdien i dropdown'en for «Andet …»; så står tidszonen i feltet tz_other.
const tzOther = "other"

// tzLabel er det danske navn på en tidszone fra listen, ellers selve IANA-navnet.
func tzLabel(zone string) string {
	for _, z := range updateTZs {
		if z.Zone == zone {
			return z.Label
		}
	}
	return zone
}

// utcOffset er tidszonens forskel til UTC lige nu, fx «UTC+1» eller «UTC−5» (følger sommertid).
func utcOffset(loc *time.Location, now time.Time) string {
	_, off := now.In(loc).Zone()
	if off == 0 {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign, off = "−", -off
	}
	h, m := off/3600, off%3600/60
	if m != 0 {
		return fmt.Sprintf("UTC%s%d:%02d", sign, h, m)
	}
	return fmt.Sprintf("UTC%s%d", sign, h)
}

// tzFromForm læser tidszonen fra formularen: dropdown'en, eller feltet tz_other, når «Andet …» er valgt.
func tzFromForm(choice, other string) (string, error) {
	if choice != tzOther {
		return choice, nil
	}
	if strings.TrimSpace(other) == "" {
		return "", errors.New("skriv tidszonen i feltet under «Andet …», fx Europe/Copenhagen")
	}
	return other, nil
}

// loadTZ læser en IANA-tidszone. Tom giver standarden. «Local» afvises: det er serverens ur, som ejeren
// ikke kan se, og netop det, tidszonen skal gøre os uafhængige af.
func loadTZ(name string) (*time.Location, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultUpdateTZ
	}
	if strings.EqualFold(name, "local") {
		return nil, "", errors.New("vælg en bestemt tidszone, fx Europe/Copenhagen")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", fmt.Errorf("ukendt tidszone: %q (skriv den som fx Europe/Copenhagen)", name)
	}
	return loc, loc.String(), nil
}

// updateWindow er, hvor længe efter det valgte tidspunkt opdateringen må gå i gang.
const updateWindow = 2 * time.Hour

// Standard, når ejeren ikke har valgt andet: hver nat kl. 03:00.
const (
	defaultUpdateDay = "daily"
	defaultUpdateAt  = "03:00"
)

// updateDays er valgene for dag, i den rækkefølge de vises. Nøglerne gemmes i Settings.UpdateDay.
var updateDays = []struct {
	Key, Label string
	Weekday    time.Weekday // bruges ikke for «daily»
}{
	{"daily", "Hver dag", 0},
	{"mon", "Mandag", time.Monday},
	{"tue", "Tirsdag", time.Tuesday},
	{"wed", "Onsdag", time.Wednesday},
	{"thu", "Torsdag", time.Thursday},
	{"fri", "Fredag", time.Friday},
	{"sat", "Lørdag", time.Saturday},
	{"sun", "Søndag", time.Sunday},
}

// schedule er et tidspunkt for opdateringer: en dag (daily eller en ugedag), et klokkeslæt og en tidszone.
type schedule struct {
	day          string // nøgle fra updateDays
	hour, minute int
	loc          *time.Location
	tz           string // tidszonens navn, fx Europe/Copenhagen
}

// parseSchedule læser dag, klokkeslæt («HH:MM») og tidszone fra formularen eller databasen.
// Tomme værdier giver standarden, hver dag kl. 03:00 i Europe/Copenhagen.
func parseSchedule(day, at, tz string) (schedule, error) {
	day, at = strings.TrimSpace(day), strings.TrimSpace(at)
	if day == "" {
		day = defaultUpdateDay
	}
	if at == "" {
		at = defaultUpdateAt
	}
	known := false
	for _, d := range updateDays {
		if d.Key == day {
			known = true
		}
	}
	if !known {
		return schedule{}, fmt.Errorf("ukendt dag: %q", day)
	}
	t, err := time.Parse("15:04", at)
	if err != nil {
		return schedule{}, errors.New("skriv klokkeslættet som TT:MM, fx 03:00")
	}
	loc, name, err := loadTZ(tz)
	if err != nil {
		return schedule{}, err
	}
	return schedule{day: day, hour: t.Hour(), minute: t.Minute(), loc: loc, tz: name}, nil
}

// schedule er ejerens valgte tidspunkt (standarden, hvis det gemte ikke kan læses).
func (s Settings) schedule() schedule {
	if sc, err := parseSchedule(s.UpdateDay, s.UpdateAt, s.UpdateTZ); err == nil {
		return sc
	}
	sc, _ := parseSchedule("", "", "")
	return sc
}

// allows er sand, hvis opdateringer må ske på datoen t (i tidspunktets tidszone).
func (sc schedule) allows(t time.Time) bool {
	if sc.day == "daily" {
		return true
	}
	for _, d := range updateDays {
		if d.Key == sc.day {
			return t.Weekday() == d.Weekday
		}
	}
	return false
}

// slotsAround er tidspunkterne fra to dage før til otte dage efter t, i tidspunktets tidszone.
func (sc schedule) slotsAround(t time.Time) []time.Time {
	local := t.In(sc.loc)
	var out []time.Time
	for i := -2; i <= 8; i++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+i, 12, 0, 0, 0, sc.loc)
		if !sc.allows(day) {
			continue
		}
		out = append(out, time.Date(day.Year(), day.Month(), day.Day(), sc.hour, sc.minute, 0, 0, sc.loc))
	}
	return out
}

// current er det tidspunkt, hvis vindue now ligger i (ok er false uden for et vindue).
func (sc schedule) current(now time.Time) (start time.Time, ok bool) {
	for _, s := range sc.slotsAround(now) {
		if !now.Before(s) && now.Before(s.Add(updateWindow)) {
			return s, true
		}
	}
	return time.Time{}, false
}

// due er sand, når en automatisk opdatering må gå i gang nu: now er i et vindue, og der er ikke
// allerede prøvet i det vindue.
func (sc schedule) due(now, lastTry time.Time) bool {
	start, ok := sc.current(now)
	return ok && lastTry.Before(start)
}

// next er det næste tidspunkt, en automatisk opdatering kan gå i gang: nu, hvis vi er i et vindue,
// der ikke er brugt, ellers det næste valgte tidspunkt.
func (sc schedule) next(now, lastTry time.Time) time.Time {
	if sc.due(now, lastTry) {
		return now
	}
	for _, s := range sc.slotsAround(now) {
		if s.After(now) {
			return s
		}
	}
	return time.Time{}
}

// describe er tidspunktet i ord, fx «hver dag kl. 03:00» eller «hver søndag kl. 03:00».
func (sc schedule) describe() string {
	at := fmt.Sprintf("kl. %02d:%02d", sc.hour, sc.minute)
	if sc.day == "daily" {
		return "hver dag " + at
	}
	for _, d := range updateDays {
		if d.Key == sc.day {
			return "hver " + strings.ToLower(d.Label) + " " + at
		}
	}
	return at
}

// danishWhen skriver et tidspunkt på dansk i tidszonen loc, fx «søndag 26. oktober kl. 03:00».
func danishWhen(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	days := []string{"søndag", "mandag", "tirsdag", "onsdag", "torsdag", "fredag", "lørdag"}
	months := []string{"januar", "februar", "marts", "april", "maj", "juni", "juli", "august", "september", "oktober", "november", "december"}
	return fmt.Sprintf("%s %d. %s kl. %02d:%02d", days[t.Weekday()], t.Day(), months[t.Month()-1], t.Hour(), t.Minute())
}
