package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingBackend er en «container», der tæller nye TCP-forbindelser.
func countingBackend(t *testing.T) (port int, newConns *atomic.Int64) {
	t.Helper()
	newConns = &atomic.Int64{}
	be := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // så forespørgslerne overlapper
		io.WriteString(w, "ok")
	}))
	be.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	be.Start()
	t.Cleanup(be.Close)
	u, _ := url.Parse(be.URL)
	port, _ = strconv.Atoi(u.Port())
	return port, newConns
}

// burst sender n samtidige forespørgsler gennem proxyen.
func burst(t *testing.T, front string, n int) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n}}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(front)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
}

// TestAppProxyReusesConnections: efter første bølge skal næste bølge genbruge forbindelserne
// til containeren i stedet for at åbne nye.
func TestAppProxyReusesConnections(t *testing.T) {
	port, newConns := countingBackend(t)
	front := httptest.NewServer(newAppProxy(port))
	defer front.Close()

	burst(t, front.URL, 20)
	first := newConns.Load()
	burst(t, front.URL, 20)
	second := newConns.Load() - first
	t.Logf("nye forbindelser: første bølge %d, anden bølge %d", first, second)
	if second > 2 {
		t.Fatalf("anden bølge åbnede %d nye forbindelser; forventede genbrug", second)
	}
}

// TestDefaultTransportChurn viser problemet før ændringen (standardtransporten med 2 ledige pr. adresse).
func TestDefaultTransportChurn(t *testing.T) {
	port, newConns := countingBackend(t)
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = http.DefaultTransport.(*http.Transport).Clone()
	front := httptest.NewServer(rp)
	defer front.Close()

	burst(t, front.URL, 20)
	first := newConns.Load()
	burst(t, front.URL, 20)
	t.Logf("standardtransport: første bølge %d, anden bølge %d nye forbindelser", first, newConns.Load()-first)
}

// TestAppProxyBadGateway: en container, der ikke svarer, giver 502 med den danske besked.
func TestAppProxyBadGateway(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	rec := httptest.NewRecorder()
	newAppProxy(port).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, ville have 502", rec.Code)
	}
}
