package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHubNeverBlocks(t *testing.T) {
	var h hub
	ch, cancel := h.subscribe("blog")
	for i := 0; i < 1000; i++ { // ingen læser: publish må ikke blokere
		h.publish("blog", event{kind: "log", data: "x"})
	}
	if len(ch) != cap(ch) || h.listeners("blog") != 1 {
		t.Fatalf("kanalen skal være fuld, ikke blokeret: %d/%d", len(ch), cap(ch))
	}
	h.publish("andet", event{kind: "status"}) // ingen lyttere: intet sker
	cancel()
	if h.listeners("blog") != 0 {
		t.Fatal("cancel skal fjerne lytteren")
	}
}

// sseClient læser beskeder (event-navn og data) fra en SSE-strøm.
type sseClient struct {
	resp *http.Response
	sc   *bufio.Scanner
}

func (c *sseClient) next(t *testing.T) (string, string) {
	t.Helper()
	var name, data string
	for c.sc.Scan() {
		line := c.sc.Text()
		switch {
		case line == "" && name != "":
			return name, data
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("strømmen sluttede: %v", c.sc.Err())
	return "", ""
}

func TestAppEventsStream(t *testing.T) {
	s, owner, _ := newResetServer(t)
	if _, err := s.mgr.Deploy("Blog", 8080, map[string]string{"index.html": "hej"}, "", owner.ID); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.handler()}
	srv.RegisterOnShutdown(s.beginShutdown)
	go srv.Serve(ln)

	open := func(slug string, cookie string) *http.Response {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/apps/"+slug+"/events", nil)
		req.Host = "min-server.dk"
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// Uden login: ingen strøm. En anden brugers app: ingen strøm.
	if r := open("blog", ""); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("uden login: %d", r.StatusCode)
	}
	if r := open("blog", "s-m"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("medlem uden adgang til appen: %d", r.StatusCode)
	}

	resp := open("blog", "s-o")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type: %s", ct)
	}
	c := &sseClient{resp: resp, sc: bufio.NewScanner(resp.Body)}
	if name, data := c.next(t); name != "status" || !strings.Contains(data, `"status":"waiting"`) {
		t.Fatalf("første besked skal være status: %s %s", name, data)
	}

	// En ændring i status og en ny logline kommer frem med det samme.
	s.mgr.setStatus("blog", StatusBuilding, "Bygger image …")
	if name, data := c.next(t); name != "status" || !strings.Contains(data, `"text":"Bygger image …"`) || !strings.Contains(data, `"busy":true`) {
		t.Fatalf("ny status: %s %s", name, data)
	}
	s.mgr.logger("blog")("Trin 2 af 4\nmed linjeskift")
	if name, data := c.next(t); name != "log" || !strings.HasSuffix(data, "Trin 2 af 4 med linjeskift") {
		t.Fatalf("ny logline: %s %q", name, data)
	}

	// Nedlukning: strømmen slutter med det samme, så Shutdown ikke venter på den.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("nedlukningen ventede på live-strømmen i %s", d)
	}
	if s.mgr.events.listeners("blog") != 0 {
		t.Fatal("lytteren skal være fjernet efter nedlukning")
	}
}

func TestAppEventsGone(t *testing.T) {
	s, owner, _ := newResetServer(t)
	if _, err := s.mgr.Deploy("Blog", 8080, map[string]string{"index.html": "hej"}, "", owner.ID); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: s.handler()}
	go srv.Serve(ln)
	defer srv.Close()
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/apps/blog/events", nil)
	req.Host = "min-server.dk"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "s-o"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	c := &sseClient{resp: resp, sc: bufio.NewScanner(resp.Body)}
	c.next(t)
	if err := s.mgr.Reject("blog"); err != nil {
		t.Fatal(err)
	}
	if name, _ := c.next(t); name != "gone" {
		t.Fatalf("en slettet app skal give «gone», fik %s", name)
	}
}

func TestHomeEvents(t *testing.T) {
	s, owner, _ := newResetServer(t)
	if _, err := s.mgr.Deploy("Blog", 8080, map[string]string{"index.html": "hej"}, "", owner.ID); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: s.handler()}
	srv.RegisterOnShutdown(s.beginShutdown)
	go srv.Serve(ln)
	defer srv.Close()
	open := func(cookie string) *sseClient {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/events", nil)
		req.Host = "min-server.dk"
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("/events: %v %v", resp, err)
		}
		return &sseClient{resp: resp, sc: bufio.NewScanner(resp.Body)}
	}
	ownerStream, memberStream := open("s-o"), open("s-m")
	for i := 0; i < 50 && s.mgr.events.listeners(allApps) < 2; i++ {
		time.Sleep(20 * time.Millisecond)
	}

	// Ejerens app skifter status: ejerens forside får besked, medlemmets ikke (appen er privat).
	s.mgr.setStatus("blog", StatusBuilding, "Bygger image …")
	if name, data := ownerStream.next(t); name != "changed" || data != "{}" {
		t.Fatalf("ejeren: %s %s", name, data)
	}
	got := make(chan string, 1)
	go func() {
		for memberStream.sc.Scan() {
			if strings.HasPrefix(memberStream.sc.Text(), "event: ") {
				got <- memberStream.sc.Text()
				return
			}
		}
	}()
	select {
	case e := <-got:
		t.Fatalf("medlemmet må ikke høre om ejerens private app: %s", e)
	case <-time.After(300 * time.Millisecond):
	}

	// Loglinjer giver ingen besked til forsiden.
	s.mgr.logger("blog")("en logline")
	s.mgr.setStatus("blog", StatusLive, "")
	if name, _ := ownerStream.next(t); name != "changed" {
		t.Fatalf("ny status skal give «changed», fik %s", name)
	}
}
