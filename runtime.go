package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runtime er det, der bygger og kører apps. Docker er til rigtig brug,
// demo-runtime'n lader dig afprøve hele flowet uden Docker.
type Runtime interface {
	Name() string
	Build(ctx context.Context, slug string, n int, dir string, logf func(string, ...any)) (image string, err error)
	Pull(ctx context.Context, slug string, n int, ref ImageRef, user, token string, logf func(string, ...any)) (image string, err error)
	Start(slug, image string, containerPort, hostPort int, env map[string]string, dir, volume string, logf func(string, ...any)) error
	RemoveImage(image string)
	Stop(slug string) error
	Logs(slug string, lines int) string
}

// ---------- Docker ----------

// dockerRT kører apps som Docker-containere med navnet vd-<slug>.
type dockerRT struct{ dataDir string }

// Name returnerer runtime'ns navn, som vises ved opstart.
func (d *dockerRT) Name() string { return "docker" }

// Build kører «docker build» på appens mappe og sender byggeloggen linje for linje til logf.
// Imaget får navnet vd-<slug>:v<n>.
func (d *dockerRT) Build(ctx context.Context, slug string, n int, dir string, logf func(string, ...any)) (string, error) {
	image := fmt.Sprintf("vd-%s:v%d", slug, n)
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", image, dir)
	w := &lineWriter{f: func(s string) { logf("%s", s) }}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	w.Flush()
	if err != nil {
		return "", fmt.Errorf("docker build fejlede: %w", err)
	}
	return image, nil
}

// Pull henter et færdigt image og giver det navnet vd-<slug>:v<n>, så rollback og oprydning
// virker som for byggede images. Hver hentning får sin egen tomme Docker-konfiguration med kun
// appens login (hvis det er sat): serverens egne logins bruges aldrig, og intet gemmes bagefter.
func (d *dockerRT) Pull(ctx context.Context, slug string, n int, ref ImageRef, user, token string, logf func(string, ...any)) (string, error) {
	runDir := filepath.Join(d.dataDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", err
	}
	cfg, err := os.MkdirTemp(runDir, "registry-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(cfg)
	auths := map[string]any{}
	if user != "" && token != "" {
		auths[ref.authKey()] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + token))}
	}
	b, _ := json.Marshal(map[string]any{"auths": auths})
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), b, 0o600); err != nil {
		return "", err
	}
	env := append(os.Environ(), "DOCKER_CONFIG="+cfg)
	if os.Getenv("DOCKER_HOST") == "" {
		// Uden serverens egen konfiguration kender docker ikke den valgte context; slå adressen op først.
		if out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
			if h := strings.TrimSpace(string(out)); h != "" {
				env = append(env, "DOCKER_HOST="+h)
			}
		}
	}

	logf("Henter %s", ref)
	var out bytes.Buffer
	w := &lineWriter{f: func(s string) { logf("%s", s) }}
	cmd := exec.CommandContext(ctx, "docker", "pull", "--quiet", ref.String())
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = io.MultiWriter(w, &out), io.MultiWriter(w, &out)
	err = cmd.Run()
	w.Flush()
	if err != nil {
		l := strings.ToLower(out.String())
		if strings.Contains(l, "pull access denied") || strings.Contains(l, "unauthorized") ||
			strings.Contains(l, "authentication required") || strings.Contains(l, "denied:") {
			return "", fmt.Errorf("docker pull fejlede: %w", errRegistryAuth)
		}
		return "", fmt.Errorf("docker pull fejlede: %w", err)
	}
	image := fmt.Sprintf("vd-%s:v%d", slug, n)
	if out, err := exec.Command("docker", "tag", ref.String(), image).CombinedOutput(); err != nil {
		logf("%s", strings.TrimSpace(string(out)))
		return "", fmt.Errorf("docker tag fejlede: %w", err)
	}
	return image, nil
}

// RemoveImage sletter et gammelt image (fejl ignoreres).
func (d *dockerRT) RemoveImage(image string) { _ = exec.Command("docker", "rmi", image).Run() }

// Start erstatter appens container med en ny fra image. Containeren får højst 512 MB og 1 CPU,
// appens datamappe monteres på /data, og porten publiceres kun på 127.0.0.1.
func (d *dockerRT) Start(slug, image string, cport, hport int, env map[string]string, dir, volume string, logf func(string, ...any)) error {
	name := "vd-" + slug
	_ = exec.Command("docker", "rm", "-f", name).Run()

	// Miljøvariabler går via en midlertidig fil, så de ikke står i procesoversigten.
	envFile := filepath.Join(d.dataDir, "run", slug+".env")
	if err := os.MkdirAll(filepath.Dir(envFile), 0o700); err != nil {
		return err
	}
	var sb strings.Builder
	keys := sortedKeys(env)
	for _, k := range keys {
		sb.WriteString(k + "=" + env[k] + "\n")
	}
	if err := os.WriteFile(envFile, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	defer os.Remove(envFile)

	out, err := exec.Command("docker", "run", "-d",
		"--name", name,
		"--restart", "unless-stopped",
		"--memory", "512m", "--cpus", "1",
		"--env-file", envFile,
		"-v", volume+":/data",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", hport, cport),
		image).CombinedOutput()
	if err != nil {
		logf("%s", strings.TrimSpace(string(out)))
		return fmt.Errorf("docker run fejlede: %w", err)
	}
	return nil
}

// Stop fjerner appens container.
func (d *dockerRT) Stop(slug string) error {
	return exec.Command("docker", "rm", "-f", "vd-"+slug).Run()
}

// Logs henter de seneste linjer fra containerens stdout/stderr.
func (d *dockerRT) Logs(slug string, lines int) string {
	out, _ := exec.Command("docker", "logs", "--tail", fmt.Sprint(lines), "vd-"+slug).CombinedOutput()
	return string(out)
}

// ---------- Demo ----------

// demoRT efterligner Docker uden containere: «bygningen» er en pause, og «containeren» er en lille
// HTTP-server i samme proces, der serverer appens index.html eller en standardside.
type demoRT struct {
	mu      sync.Mutex
	servers map[string]*http.Server
	logs    map[string][]string
}

// newDemoRT laver en tom demo-runtime.
func newDemoRT() *demoRT {
	return &demoRT{servers: map[string]*http.Server{}, logs: map[string][]string{}}
}

// Name returnerer runtime'ns navn, som vises ved opstart.
func (d *demoRT) Name() string { return "demo" }

// Build lader, som om den bygger, og returnerer et falsk image-navn.
func (d *demoRT) Build(ctx context.Context, slug string, n int, dir string, logf func(string, ...any)) (string, error) {
	logf("[demo] Læser filer i %s", dir)
	time.Sleep(700 * time.Millisecond)
	logf("[demo] Bygger image (spillet, ingen Docker i brug)")
	time.Sleep(900 * time.Millisecond)
	return fmt.Sprintf("demo/%s:v%d", slug, n), nil
}

// Pull lader, som om den henter et færdigt image, og returnerer et falsk image-navn.
func (d *demoRT) Pull(ctx context.Context, slug string, n int, ref ImageRef, user, token string, logf func(string, ...any)) (string, error) {
	logf("[demo] Henter %s (spillet, ingen Docker i brug)", ref)
	time.Sleep(700 * time.Millisecond)
	return fmt.Sprintf("demo/%s:v%d", slug, n), nil
}

// RemoveImage gør intet i demo-tilstand.
func (d *demoRT) RemoveImage(image string) {}

// Start serverer appens mappe (eller en standardside, hvis der ikke er en index.html)
// på 127.0.0.1:hport og logger hver forespørgsel.
func (d *demoRT) Start(slug, image string, cport, hport int, env map[string]string, dir, volume string, logf func(string, ...any)) error {
	_ = d.Stop(slug)
	mux := http.NewServeMux()
	index := filepath.Join(dir, "index.html")
	_, statErr := os.Stat(index)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.addLog(slug, fmt.Sprintf("%s %s %s", time.Now().Format("15:04:05"), r.Method, r.URL.Path))
		if statErr == nil {
			http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
			return
		}
		keys := sortedKeys(env)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>%s</title>
<body style="font:18px system-ui;max-width:640px;margin:15vh auto;padding:0 20px">
<h1>%s</h1><p>Dette er en demo-app (%s). Med Docker kører din rigtige container her.</p>
<p>Variabelnavne appen kan se: <code>%s</code></p></body>`,
			html.EscapeString(slug), html.EscapeString(slug), html.EscapeString(image), html.EscapeString(strings.Join(keys, ", ")))
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hport))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	d.mu.Lock()
	d.servers[slug] = srv
	d.mu.Unlock()
	go srv.Serve(ln)
	logf("[demo] App lytter på 127.0.0.1:%d", hport)
	return nil
}

// Stop lukker appens demo-server, hvis den kører.
func (d *demoRT) Stop(slug string) error {
	d.mu.Lock()
	srv := d.servers[slug]
	delete(d.servers, slug)
	d.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return nil
}

// addLog gemmer en logline; kun de seneste 200 beholdes pr. app.
func (d *demoRT) addLog(slug, line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := append(d.logs[slug], line)
	if len(l) > 200 {
		l = l[len(l)-200:]
	}
	d.logs[slug] = l
}

// Logs returnerer de seneste linjer fra demo-loggen.
func (d *demoRT) Logs(slug string, lines int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.logs[slug]
	if len(l) > lines {
		l = l[len(l)-lines:]
	}
	return strings.Join(l, "\n")
}

// ---------- hjælpere ----------

// lineWriter deler en strøm (fx output fra docker build) op i hele linjer og kalder f for hver.
type lineWriter struct {
	f   func(string)
	buf []byte
}

// Write samler bytes og kalder f for hver hel linje.
func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.f(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush sender en sidste, ufærdig linje videre.
func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.f(string(w.buf))
		w.buf = nil
	}
}

// sortedKeys returnerer nøglerne i m sorteret, så rækkefølgen er stabil.
func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

// freePort beder styresystemet om en ledig port på 127.0.0.1 til en ny app.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy venter, til appen svarer på /health på port, eller til timeout er gået.
// Ethvert svar under 500 tæller, så apps uden en /health-side (404) også godkendes, så snart de svarer;
// en app med /health kan selv sige «ikke klar endnu» med 503.
func waitHealthy(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
