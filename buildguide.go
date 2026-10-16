package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// guideRules er de regler, en app skal følge for at køre på serveren. De sendes til AI'en med
// get_build_guide og står kort i MCP-instruktionerne, så udgivelser lykkes første gang.
const guideRules = `Byggevejledning til Vibe Deploy

Følg disse regler, så virker udgivelsen som regel første gang:

1. Dockerfile i roden. Byg med flere trin (multi-stage), hvis der skal kompileres; det endelige image skal være lille.
2. Lyt på 0.0.0.0 (ikke localhost/127.0.0.1) og på porten fra miljøvariablen PORT. Angiv samme port i udgivelseskaldet (standard 8080).
3. Data, der skal overleve, gemmes i mappen fra DATA_DIR (/data). Alt andet i containeren forsvinder ved genstart og ny version. Brug SQLite i /data i stedet for en separat database.
4. Lav en /health-side, der svarer 200 «ok» hurtigt og uden login, når appen er klar. Serveren venter på den ved opstart (højst 40 sekunder).
5. Ingen hemmeligheder i filerne. Læs dem fra miljøvariabler, og opret pladsen med request_secret; ejeren indsætter værdien i dashboardet. Almindelige indstillinger sættes med set_env.
6. Én container og én port pr. app. Ingen docker-compose og ingen ekstra tjenester.
7. Appen får højst 512 MB hukommelse og 1 CPU.
8. Log til stdout/stderr; get_logs viser det.
9. Serveren er typisk linux/amd64. Færdige images (deploy_image) skal være bygget til den.
10. Lås versioner fast (fx node:22-alpine, package-lock.json, requirements.txt med versioner), så en ny udgivelse ikke pludselig bygger anderledes.

Efter udgivelsen: brug get_status, til appen er live. Fejler den, så hent get_logs, ret fejlen og udgiv igen. Den forrige version kører videre imens.`

// guideStacks er skabeloner til de mest almindelige slags apps. Nøglen er værdien af «stack».
var guideStacks = map[string]string{
	"static": `Statisk side (HTML/CSS/JS) med nginx. Læg siden i mappen site/. Udgiv med port 8080.

--- Dockerfile ---
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY site/ /usr/share/nginx/html/

--- nginx.conf ---
server {
    listen 8080;
    root /usr/share/nginx/html;
    location = /health { access_log off; default_type text/plain; return 200 "ok\n"; }
    location / { try_files $uri $uri/ /index.html; }
}`,

	"node": `Node.js (her med Express). Kræver package.json og package-lock.json (npm ci). Udgiv med port 8080.

--- Dockerfile ---
FROM node:22-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --omit=dev
COPY . .
ENV NODE_ENV=production
CMD ["node", "server.js"]

--- server.js (det vigtige) ---
const express = require("express");
const path = require("path");
const app = express();
const port = process.env.PORT || 8080;
const dataDir = process.env.DATA_DIR || "./data";   // gem filer og databaser her
app.get("/health", (req, res) => res.send("ok"));
app.listen(port, "0.0.0.0", () => console.log("lytter på", port));

Til en database: better-sqlite3 med filen path.join(dataDir, "app.db").`,

	"python": `Python (her med Flask og gunicorn). Kræver requirements.txt med Flask og gunicorn. Udgiv med port 8080.

--- Dockerfile ---
FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
CMD ["sh", "-c", "exec gunicorn -b 0.0.0.0:${PORT:-8080} -w 2 app:app"]

--- app.py (det vigtige) ---
import os, sqlite3
from flask import Flask
app = Flask(__name__)
DATA_DIR = os.environ.get("DATA_DIR", "./data")   # gem filer og databaser her
db = lambda: sqlite3.connect(os.path.join(DATA_DIR, "app.db"))

@app.get("/health")
def health():
    return "ok"`,

	"go": `Go med kun standardbiblioteket. Udgiv med port 8080.

--- Dockerfile ---
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /app .

FROM alpine:3.20
COPY --from=build /app /app
CMD ["/app"]

--- main.go (det vigtige) ---
port := os.Getenv("PORT")
if port == "" { port = "8080" }
dataDir := os.Getenv("DATA_DIR")   // gem filer og databaser her
http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
log.Fatal(http.ListenAndServe("0.0.0.0:"+port, nil))

Til SQLite uden cgo: modernc.org/sqlite.`,
}

// guideStackNames er de gyldige værdier af «stack», sorteret.
func guideStackNames() []string {
	var n []string
	for k := range guideStacks {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// buildGuide returnerer reglerne og, hvis stack er kendt, skabelonen til den slags app.
func buildGuide(stack string) string {
	out := guideRules
	stack = strings.ToLower(strings.TrimSpace(stack))
	if t, ok := guideStacks[stack]; ok {
		return out + "\n\n" + t
	}
	return out + "\n\nSkabeloner findes til: " + strings.Join(guideStackNames(), ", ") + ". Kald get_build_guide med stack for at få en."
}

// dockerfileWarnings finder almindelige fejl i appens Dockerfile, før den bygges: en EXPOSE-port,
// der ikke passer med den angivne port, og en app, der kun lytter på localhost.
// Advarslerne stopper ikke udgivelsen, men sendes med i svaret til AI'en.
func dockerfileWarnings(dir string, port int) []string {
	f, err := os.Open(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var warns []string
	var exposed []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if strings.EqualFold(fields[0], "EXPOSE") {
			for _, p := range fields[1:] {
				if n, err := strconv.Atoi(strings.Split(p, "/")[0]); err == nil {
					exposed = append(exposed, n)
				}
			}
		}
		if (strings.EqualFold(fields[0], "CMD") || strings.EqualFold(fields[0], "ENTRYPOINT")) &&
			(strings.Contains(line, "127.0.0.1") || strings.Contains(line, "localhost")) {
			warns = append(warns, "CMD/ENTRYPOINT nævner localhost/127.0.0.1. Appen skal lytte på 0.0.0.0, ellers kan serveren ikke nå den.")
		}
	}
	if len(exposed) > 0 {
		found := false
		for _, p := range exposed {
			found = found || p == port
		}
		if !found {
			warns = append(warns, fmt.Sprintf("Dockerfile har EXPOSE %v, men udgivelsen bruger port %d. Appen skal lytte på den port, der er angivet i udgivelsen (miljøvariablen PORT).", exposed, port))
		}
	}
	return warns
}
