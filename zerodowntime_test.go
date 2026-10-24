package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hammer sender forespørgsler til appen, som proxyen gør (HostPort læses ved hver forespørgsel),
// indtil stop lukkes. Den tæller fejl og husker, hvilke svar den har set.
func hammer(t *testing.T, m *Manager, slug string, stop <-chan struct{}) (fails *int64, seen func() map[string]bool, done <-chan struct{}) {
	t.Helper()
	var n int64
	var mu sync.Mutex
	bodies := map[string]bool{}
	fin := make(chan struct{})
	client := &http.Client{Timeout: 2 * time.Second}
	go func() {
		defer close(fin)
		for {
			select {
			case <-stop:
				return
			default:
			}
			a, _ := m.get(slug)
			if !serving(a) {
				atomic.AddInt64(&n, 1)
				continue
			}
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", a.HostPort))
			if err != nil {
				atomic.AddInt64(&n, 1)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				atomic.AddInt64(&n, 1)
				continue
			}
			mu.Lock()
			bodies[strings.TrimSpace(string(b))] = true
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return &n, func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]bool{}
		for k := range bodies {
			out[k] = true
		}
		return out
	}, fin
}

func portOpen(port int) bool {
	c := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func deployHTML(t *testing.T, m *Manager, body string) {
	t.Helper()
	if _, err := m.Deploy("Bageri", 8080, map[string]string{"index.html": body}, body, "u1"); err != nil {
		t.Fatal(err)
	}
}

func waitCurrent(t *testing.T, m *Manager, slug string, n int) App {
	t.Helper()
	for i := 0; i < 200; i++ {
		if a, _ := m.get(slug); a.Current == n && a.Status == StatusLive {
			return a
		}
		time.Sleep(50 * time.Millisecond)
	}
	a, _ := m.get(slug)
	t.Fatalf("version %d blev ikke live: current=%d status=%q (%s)", n, a.Current, a.Status, a.StatusText)
	return a
}

// En ny version, en genstart og en rollback skifter alle uden en eneste fejlet forespørgsel,
// og den gamle container lukkes bagefter.
func TestZeroDowntimeDeploy(t *testing.T) {
	old := drainDelay
	drainDelay = 300 * time.Millisecond
	defer func() { drainDelay = old }()

	m := newTestManager(t)
	deployHTML(t, m, "v1")
	if err := m.Approve("bageri"); err != nil {
		t.Fatal(err)
	}
	a1 := waitCurrent(t, m, "bageri", 1)
	defer m.stopAll("bageri")

	stop := make(chan struct{})
	fails, seen, done := hammer(t, m, "bageri", stop)

	deployHTML(t, m, "v2") // bygges, mens v1 stadig får trafik
	a2 := waitCurrent(t, m, "bageri", 2)
	if a2.Slot == a1.Slot || a2.HostPort == a1.HostPort {
		t.Fatalf("v2 burde køre på den anden plads og en ny port: v1 %q:%d, v2 %q:%d", a1.Slot, a1.HostPort, a2.Slot, a2.HostPort)
	}
	time.Sleep(drainDelay + 500*time.Millisecond)
	if portOpen(a1.HostPort) {
		t.Fatal("den gamle container burde være lukket efter drainDelay")
	}

	if err := m.Restart("bageri"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	a3 := waitCurrent(t, m, "bageri", 2)
	if a3.Slot == a2.Slot {
		t.Fatal("en genstart burde også skifte plads")
	}

	if err := m.Rollback("bageri", 1); err != nil {
		t.Fatal(err)
	}
	waitCurrent(t, m, "bageri", 1)
	time.Sleep(200 * time.Millisecond)

	close(stop)
	<-done
	if n := atomic.LoadInt64(fails); n != 0 {
		t.Fatalf("%d forespørgsler fejlede under udrulningerne", n)
	}
	got := seen()
	if !got["v1"] || !got["v2"] {
		t.Fatalf("burde have set både v1 og v2, så: %v", got)
	}
}

// failRT er demo-runtime'n, men version 3 starter aldrig.
type failRT struct{ *demoRT }

func (f failRT) Start(inst, image string, cport, hport int, env map[string]string, dir, volume string, logf func(string, ...any)) error {
	if strings.HasSuffix(image, ":v3") {
		return errors.New("crash ved opstart")
	}
	return f.demoRT.Start(inst, image, cport, hport, env, dir, volume, logf)
}

func (f failRT) Build(ctx context.Context, slug string, n int, dir string, logf func(string, ...any)) (string, error) {
	return fmt.Sprintf("demo/%s:v%d", slug, n), nil
}

// Fejler en ny version, mærker de besøgende intet: den gamle blev aldrig stoppet.
func TestFailedVersionKeepsOldRunning(t *testing.T) {
	old := drainDelay
	drainDelay = 100 * time.Millisecond
	defer func() { drainDelay = old }()

	m := newTestManager(t)
	m.rt = failRT{newDemoRT()}
	deployHTML(t, m, "v1")
	_ = m.Approve("bageri")
	waitCurrent(t, m, "bageri", 1)
	deployHTML(t, m, "v2")
	a2 := waitCurrent(t, m, "bageri", 2)
	defer m.stopAll("bageri")

	stop := make(chan struct{})
	fails, seen, done := hammer(t, m, "bageri", stop)
	deployHTML(t, m, "v3")
	var a App
	for i := 0; i < 100; i++ {
		if a, _ = m.get("bageri"); len(a.Versions) == 3 && a.Versions[2].Failed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-done

	a, _ = m.get("bageri")
	if a.Current != 2 || a.Status != StatusLive || a.Slot != a2.Slot || a.HostPort != a2.HostPort {
		t.Fatalf("v2 burde køre uændret: %+v", a)
	}
	if !strings.Contains(a.StatusText, "Version 3 fejlede") {
		t.Fatalf("ejeren burde få at vide, at v3 fejlede: %q", a.StatusText)
	}
	if n := atomic.LoadInt64(fails); n != 0 {
		t.Fatalf("%d forespørgsler fejlede", n)
	}
	if got := seen(); len(got) != 1 || !got["v2"] {
		t.Fatalf("kun v2 burde have svaret: %v", got)
	}
}

func TestServing(t *testing.T) {
	cases := []struct {
		status  string
		current int
		want    bool
	}{
		{StatusLive, 1, true},
		{StatusBuilding, 1, true}, // forrige version kører, mens den nye bygges
		{StatusStarting, 2, true},
		{StatusBuilding, 0, false}, // første version: intet at sende videre til
		{StatusWaiting, 0, false},
		{StatusStopped, 1, false},
		{StatusFailed, 1, false},
	}
	for _, c := range cases {
		if got := serving(App{Status: c.status, Current: c.current}); got != c.want {
			t.Errorf("serving(%s, current=%d) = %v, ventede %v", c.status, c.current, got, c.want)
		}
	}
}

func TestInstNames(t *testing.T) {
	if instName("blog", "") != "vd-blog" || instName("blog", "b") != "vd-blog_b" {
		t.Fatal("forkerte containernavne")
	}
	if otherSlot("") != "b" || otherSlot("b") != "" {
		t.Fatal("pladserne burde skifte")
	}
	for _, n := range []string{"vd-min-blog", "vd-min-blog_b"} {
		if appOf(n) != "min-blog" {
			t.Fatalf("appOf(%q) = %q", n, appOf(n))
		}
	}
	got := parseDockerStats("vd-blog\t1.5%\t10MiB / 512MiB\nvd-blog_b\t0.5%\t20MiB / 512MiB\n")
	if u := got["blog"]; len(got) != 1 || u.CPU != 2 || u.MemUsed != 30<<20 {
		t.Fatalf("begge pladser burde lægges sammen under app blog: %+v", got)
	}
}

// TestReconcileLeavesNewDeploysAlone: Reconcile kører, mens serveren tager imod forespørgsler. Går en ny
// udgivelse i gang, mens Reconcile tjekker en app, må Reconcile ikke overskrive dens status.
func TestReconcileLeavesNewDeploysAlone(t *testing.T) {
	m := newTestManager(t)
	_ = m.st.Do(func(db *DB) error {
		db.Apps["blog"] = &App{Slug: "blog", Name: "Blog", Status: StatusLive, Current: 1, Approved: true}
		return nil
	})
	was, _ := m.get("blog")
	m.setStatus("blog", StatusBuilding, "Pakker din app …") // en ny udgivelse når at gå i gang
	if m.setIfUnchanged("blog", was, StatusLive, "") {
		t.Fatal("Reconcile må ikke overskrive en app, der har ændret sig")
	}
	if a, _ := m.get("blog"); a.Status != StatusBuilding {
		t.Fatalf("status skal være den nye udgivelses, fik %s", a.Status)
	}
	was, _ = m.get("blog")
	if !m.setIfUnchanged("blog", was, StatusLive, "") {
		t.Fatal("uden ændringer skal status sættes")
	}
}
