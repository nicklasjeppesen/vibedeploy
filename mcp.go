package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// rpcReq er en JSON-RPC 2.0-forespørgsel fra MCP-klienten. Uden ID er det en notifikation.
type rpcReq struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// supportedProtocols er de MCP-versioner, serveren kan tale. Den første bruges,
// hvis klienten beder om en ukendt version.
var supportedProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// mcpAuth finder brugeren bag Bearer-tokenet. Kun gyldige tokens fra brugere med bygge-ret
// accepteres, så fjernes bygge-retten, virker det med det samme.
func (s *Server) mcpAuth(r *http.Request) (*User, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, false
	}
	var user *User
	s.st.View(func(db *DB) {
		t := db.Tokens[strings.TrimPrefix(h, "Bearer ")]
		if t == nil || time.Now().After(t.Expires) {
			return
		}
		if u := db.user(t.UserID); u != nil && canBuild(u) {
			cp := *u
			user = &cp
		}
	})
	return user, user != nil
}

// mcp er MCP-endpointet (Streamable HTTP uden streaming): ét JSON-RPC-kald pr. POST.
// Uden gyldigt token svares 401 med en henvisning til OAuth-metadata, så klienten kan starte login.
func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mcpAuth(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.baseURL(r)))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Kun POST understøttes", http.StatusMethodNotAllowed)
		return
	}
	var req rpcReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, rpcErr(nil, -32700, "Ugyldig JSON"))
		return
	}
	if len(req.ID) == 0 { // notifikation
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := supportedProtocols[0]
		for _, v := range supportedProtocols {
			if v == p.ProtocolVersion {
				pv = v
			}
		}
		writeJSON(w, 200, rpcOK(req.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "vibe-deploy", "version": version},
			"instructions":    "Udgiv apps som containere på ejerens egen server. Kald get_build_guide (med stack, fx node, python, go eller static), før du skriver eller udgiver en ny app: den har reglerne og en Dockerfile-skabelon. Kort: Dockerfile i roden, lyt på 0.0.0.0 og porten fra PORT, gem data i DATA_DIR (/data), lav en /health-side. Små apps: deploy_app med filerne som tekst. Større apps: prepare_upload, hvis du har en terminal, ellers deploy_from_git, hvor serveren selv henter koden fra et git-repo. Færdige images fra Docker Hub eller et andet registry: deploy_image. Hemmeligheder må aldrig sendes i chatten: brug request_secret.",
		}))
	case "ping":
		writeJSON(w, 200, rpcOK(req.ID, map[string]any{}))
	case "tools/list":
		writeJSON(w, 200, rpcOK(req.ID, map[string]any{"tools": toolDefs()}))
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeJSON(w, 200, rpcErr(req.ID, -32602, "Ugyldige parametre"))
			return
		}
		text, err := s.callTool(r, user, p.Name, p.Arguments)
		res := map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
		if err != nil {
			res["content"] = []map[string]string{{"type": "text", "text": "Fejl: " + err.Error()}}
			res["isError"] = true
		}
		writeJSON(w, 200, rpcOK(req.ID, res))
	default:
		writeJSON(w, 200, rpcErr(req.ID, -32601, "Ukendt metode"))
	}
}

// rpcOK bygger et JSON-RPC-svar.
func rpcOK(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// rpcErr bygger et JSON-RPC-fejlsvar.
func rpcErr(id json.RawMessage, code int, msg string) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": code, "message": msg}}
	if id != nil {
		m["id"] = id
	} else {
		m["id"] = nil
	}
	return m
}

// obj bygger et JSON Schema-objekt til et værktøjs input.
func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

// str bygger et JSON Schema-felt af typen string.
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// toolDefs er de MCP-værktøjer, AI'en ser (tools/list). Beskrivelserne fungerer som instruktioner til AI'en.
func toolDefs() []map[string]any {
	name := str("Appens navn, fx «Min bageri-app». Bruges til adressen.")
	return []map[string]any{
		{"name": "get_build_guide", "description": "Hent reglerne for apps på denne server og en færdig Dockerfile-skabelon. Kald den, før du skriver eller udgiver en ny app; så lykkes udgivelsen som regel første gang.",
			"inputSchema": obj(map[string]any{
				"stack": map[string]any{"type": "string", "enum": guideStackNames(), "description": "Slags app. Udelad for kun at få reglerne."},
			})},
		{"name": "list_apps", "description": "Vis ejerens apps og deres status.", "inputSchema": obj(map[string]any{})},
		{"name": "deploy_app", "description": "Udgiv en app som container. Send alle filer (tekst) inkl. en Dockerfile i roden. Appen skal lytte på den angivne port. Første udgivelse af en ny app skal godkendes af ejeren i dashboardet; svaret fortæller, om den venter.",
			"inputSchema": obj(map[string]any{
				"name":  name,
				"port":  map[string]any{"type": "integer", "description": "Porten appen lytter på i containeren (standard 8080)."},
				"files": map[string]any{"type": "object", "description": "Filsti → filindhold, fx {\"Dockerfile\": \"...\", \"main.go\": \"...\"}.", "additionalProperties": map[string]any{"type": "string"}},
				"note":  str("Kort beskrivelse af ændringen, vises i versionslisten på dansk."),
			}, "name", "files")},
		{"name": "prepare_upload", "description": "Til større apps eller apps med billeder: giver en engangsadresse og en kommando, der pakker en mappe og sender den. Kræver en terminal (fx Claude Code). Mappen skal have en Dockerfile i roden. node_modules, .git og .env sendes ikke med.",
			"inputSchema": obj(map[string]any{
				"name":   name,
				"folder": str("Stien til appens mappe på brugerens maskine (standard: nuværende mappe)."),
				"port":   map[string]any{"type": "integer", "description": "Porten appen lytter på i containeren (standard 8080)."},
				"note":   str("Kort beskrivelse af ændringen på dansk."),
			}, "name")},
		{"name": "deploy_from_git", "description": "Udgiv en app ved at lade serveren hente koden fra et git-repo (fx GitHub). Virker også fra AI-værktøjer uden terminal, og med store apps og billeder. Offentlige repos hentes direkte. Er repoet privat, svarer værktøjet med en deploy key, som ejeren skal tilføje i repoets indstillinger (kun læseadgang); kør derefter værktøjet igen. Repoet skal have en Dockerfile i roden. Til en app, der allerede er hentet fra git, kan repo udelades.",
			"inputSchema": obj(map[string]any{
				"name":   name,
				"repo":   str("Repoet, fx «ejer/repo» (GitHub), https://github.com/ejer/repo eller git@gitlab.com:ejer/repo.git."),
				"branch": str("Gren (standard: repoets standardgren)."),
				"port":   map[string]any{"type": "integer", "description": "Porten appen lytter på i containeren (standard 8080)."},
				"note":   str("Kort beskrivelse af ændringen på dansk."),
			}, "name")},
		{"name": "deploy_image", "description": "Udgiv et færdigt image fra Docker Hub, GitHub (ghcr.io) eller et andet registry uden at bygge noget, fx «nginx:1.27», «firma/app:latest» eller «ghcr.io/ejer/app:latest». Virker fra alle AI-værktøjer og kræver ingen Dockerfile. Er imaget privat, så sæt private=true: serveren opretter så felterne " + RegistryUserKey + " og " + RegistryTokenKey + " i dashboardet, hvor ejeren selv indsætter sit login (aldrig i chatten). Imaget skal være bygget til serverens processortype (typisk linux/amd64). Til en app, der allerede er et image, kan image udelades; så hentes nyeste udgave af samme tag.",
			"inputSchema": obj(map[string]any{
				"name":    name,
				"image":   str("Imaget, fx «nginx:1.27», «ejer/app:latest» eller «ghcr.io/ejer/app:v2». Uden tag bruges latest."),
				"port":    map[string]any{"type": "integer", "description": "Porten appen lytter på i containeren (standard 8080)."},
				"private": map[string]any{"type": "boolean", "description": "Sæt true, hvis imaget er privat og kræver login."},
				"note":    str("Kort beskrivelse af ændringen på dansk."),
			}, "name")},
		{"name": "get_status", "description": "Hent status, adresse, version og manglende hemmeligheder for en app.", "inputSchema": obj(map[string]any{"name": name}, "name")},
		{"name": "get_logs", "description": "Hent de seneste logliner for en app. Brug den, når en udgivelse fejler.", "inputSchema": obj(map[string]any{"name": name, "lines": map[string]any{"type": "integer", "description": "Antal linjer (standard 80)."}}, "name")},
		{"name": "set_env", "description": "Sæt almindelige miljøvariabler (IKKE hemmeligheder). Appen genstartes, så de træder i kraft.", "inputSchema": obj(map[string]any{"name": name, "vars": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}}, "name", "vars")},
		{"name": "request_secret", "description": "Bed ejeren indsætte en hemmelighed (API-nøgle, adgangskode). Opretter en tom plads i dashboardet; du kan aldrig læse værdien. Bed IKKE brugeren skrive hemmeligheden i chatten.", "inputSchema": obj(map[string]any{"name": name, "keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Navne, fx [\"STRIPE_NOEGLE\"]."}}, "name", "keys")},
		{"name": "rollback", "description": "Gå tilbage til en tidligere, velfungerende version.", "inputSchema": obj(map[string]any{"name": name, "version": map[string]any{"type": "integer"}}, "name", "version")},
		{"name": "stop_app", "description": "Sæt en app på pause.", "inputSchema": obj(map[string]any{"name": name}, "name")},
		{"name": "start_app", "description": "Start en pauset app igen (genstarter nuværende version).", "inputSchema": obj(map[string]any{"name": name}, "name")},
	}
}

// callTool udfører et værktøjskald som brugeren u. Værktøjer, der kan oprette en app
// (deploy_app, deploy_from_git, deploy_image, prepare_upload), tjekker først, at navnet ikke tilhører en anden;
// de øvrige kræver, at appen findes, og at brugeren må styre den.
func (s *Server) callTool(r *http.Request, u *User, tool string, raw json.RawMessage) (string, error) {
	var a struct {
		Name    string            `json:"name"`
		Port    int               `json:"port"`
		Files   map[string]string `json:"files"`
		Note    string            `json:"note"`
		Lines   int               `json:"lines"`
		Vars    map[string]string `json:"vars"`
		Keys    []string          `json:"keys"`
		Version int               `json:"version"`
		Folder  string            `json:"folder"`
		Repo    string            `json:"repo"`
		Branch  string            `json:"branch"`
		Image   string            `json:"image"`
		Private bool              `json:"private"`
		Stack   string            `json:"stack"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("ugyldige argumenter: %v", err)
		}
	}
	slug := slugify(a.Name)
	switch tool {
	case "get_build_guide":
		return buildGuide(a.Stack), nil
	case "list_apps":
		db := s.st.Snapshot()
		var slugs []string
		for k, ap := range db.Apps {
			if canManage(ap, u) {
				slugs = append(slugs, k)
			}
		}
		if len(slugs) == 0 {
			return "Ingen apps endnu.", nil
		}
		sort.Strings(slugs)
		var sb strings.Builder
		for _, k := range slugs {
			ap := db.Apps[k]
			fmt.Fprintf(&sb, "- %s: %s (%s)\n", ap.Name, statusLabel(ap.Status), s.appURL(k))
		}
		return sb.String(), nil
	case "deploy_app":
		if existing, found := s.mgr.get(slug); found && !canManage(&existing, u) {
			return "", fmt.Errorf("navnet «%s» er optaget; vælg et andet navn", a.Name)
		}
		res, err := s.mgr.Deploy(a.Name, a.Port, a.Files, a.Note, u.ID)
		if err != nil {
			return "", err
		}
		return s.deployMessage(r, res), nil
	case "deploy_from_git":
		existing, found := s.mgr.get(slug)
		if found && !canManage(&existing, u) {
			return "", fmt.Errorf("navnet «%s» er optaget; vælg et andet navn", a.Name)
		}
		repo, branch := a.Repo, a.Branch
		if repo == "" && found && existing.Git != nil {
			repo = existing.Git.Repo
			if branch == "" {
				branch = existing.Git.Branch
			}
		}
		if repo == "" {
			return "", errors.New("angiv repo, fx «ejer/repo» eller https://github.com/ejer/repo")
		}
		port := a.Port
		if port == 0 && found {
			port = existing.Port
		}
		res, err := s.mgr.DeployFromGit(a.Name, repo, branch, port, a.Note, u.ID)
		var nk *ErrNeedKey
		if errors.As(err, &nk) {
			return needKeyText(nk), nil
		}
		if err != nil {
			return "", err
		}
		return s.deployMessage(r, res), nil
	case "deploy_image":
		existing, found := s.mgr.get(slug)
		if found && !canManage(&existing, u) {
			return "", fmt.Errorf("navnet «%s» er optaget; vælg et andet navn", a.Name)
		}
		image, port := a.Image, a.Port
		if image == "" && found {
			image = existing.FromImage
		}
		if image == "" {
			return "", errors.New("angiv image, fx «nginx:1.27» eller «ejer/app:latest»")
		}
		if port == 0 && found {
			port = existing.Port
		}
		res, err := s.mgr.DeployImage(a.Name, image, port, a.Private, a.Note, u.ID)
		var nl *ErrNeedLogin
		if errors.As(err, &nl) {
			return registryLoginText(nl.Ref, s.dashboardURL(r)+"/apps/"+slug+"?tab=indstillinger") + " Kør derefter deploy_image igen.", nil
		}
		if err != nil {
			return "", err
		}
		msg := s.deployMessage(r, res)
		if needsRegistryLogin(res.App) {
			ref, _ := parseImage(image)
			msg += "\n\n" + registryLoginText(ref, s.dashboardURL(r)+"/apps/"+res.App.Slug+"?tab=indstillinger") + " Det skal gøres, før appen godkendes."
		}
		return msg, nil
	case "prepare_upload":
		if existing, found := s.mgr.get(slug); found && !canManage(&existing, u) {
			return "", fmt.Errorf("navnet «%s» er optaget; vælg et andet navn", a.Name)
		}
		if !slugRe.MatchString(slug) || reserved[slug] {
			return "", fmt.Errorf("navnet «%s» kan ikke bruges som adresse; vælg et andet", a.Name)
		}
		tok := randomToken(24)
		_ = s.st.Do(func(db *DB) error {
			for k, up := range db.Uploads {
				if time.Now().After(up.Expires) {
					delete(db.Uploads, k)
				}
			}
			db.Uploads[tok] = &Upload{UserID: u.ID, Name: a.Name, Note: a.Note, Port: a.Port, Expires: time.Now().Add(15 * time.Minute)}
			return nil
		})
		folder := a.Folder
		if folder == "" {
			folder = "."
		}
		up := s.baseURL(r) + "/upload/" + tok
		return fmt.Sprintf("Upload-adresse (virker én gang i 15 minutter): %s\n\nKør denne kommando i en terminal:\n\nCOPYFILE_DISABLE=1 tar -czf - --exclude=node_modules --exclude=.git --exclude=.env -C %q . | curl --fail-with-body -sS -X PUT --data-binary @- %q\n\nSvaret fortæller, om appen bygges eller venter på ejerens godkendelse. Hemmeligheder i .env sendes ikke med; brug request_secret til dem.", up, folder, up), nil
	}

	app, ok := s.mgr.get(slug)
	if !ok || !canManage(&app, u) {
		return "", fmt.Errorf("appen «%s» findes ikke; brug list_apps", a.Name)
	}
	switch tool {
	case "get_status":
		var missing []string
		for _, sec := range app.Secrets {
			if sec.Value == "" {
				missing = append(missing, sec.Key)
			}
		}
		out := fmt.Sprintf("%s: %s. Adresse: %s. Kørende version: %d.", app.Name, statusLabel(app.Status), s.appURL(slug), app.Current)
		if app.StatusText != "" {
			out += " " + app.StatusText
		}
		if len(missing) > 0 {
			out += " Mangler hemmeligheder (ejeren udfylder dem i dashboardet): " + strings.Join(missing, ", ") + "."
		}
		if app.NeedsRestart {
			out += " Der er ændringer, som kræver genstart."
		}
		if len(app.AllowIPs) > 0 {
			out += " Appen kan kun åbnes fra disse IP-adresser (ejeren ændrer det i dashboardet): " + strings.Join(app.AllowIPs, ", ") + ". Andre får 403."
		}
		return out, nil
	case "get_logs":
		if a.Lines <= 0 || a.Lines > 400 {
			a.Lines = 80
		}
		l := s.mgr.Logs(slug, a.Lines)
		if l == "" {
			l = "(ingen log endnu)"
		}
		return l, nil
	case "set_env":
		if err := s.mgr.MergeEnv(slug, a.Vars); err != nil {
			return "", err
		}
		if app.Current > 0 {
			_ = s.mgr.Restart(slug)
			return "Variablerne er gemt, og appen genstarter.", nil
		}
		return "Variablerne er gemt og bruges ved næste udgivelse.", nil
	case "request_secret":
		if err := s.mgr.RequestSecrets(slug, a.Keys); err != nil {
			return "", err
		}
		return fmt.Sprintf("Jeg har oprettet pladser til %s. Bed ejeren åbne %s/apps/%s?tab=indstillinger og indsætte værdierne. Du kan ikke læse dem, og de må ikke skrives i chatten.",
			strings.Join(a.Keys, ", "), s.dashboardURL(r), slug), nil
	case "rollback":
		if err := s.mgr.Rollback(slug, a.Version); err != nil {
			return "", err
		}
		return fmt.Sprintf("Går tilbage til version %d.", a.Version), nil
	case "stop_app":
		return "Appen er sat på pause.", s.mgr.Stop(slug)
	case "start_app":
		if err := s.mgr.Restart(slug); err != nil {
			return "", err
		}
		return "Appen starter igen.", nil
	}
	return "", fmt.Errorf("ukendt værktøj: %s", tool)
}

// deployMessage er svaret til AI'en efter en udgivelse: enten «bygges nu» eller «venter på godkendelse»,
// plus eventuelle advarsler om Dockerfile.
func (s *Server) deployMessage(r *http.Request, res *DeployResult) string {
	msg := s.deployStatus(r, res)
	if len(res.Warnings) > 0 {
		msg += "\n\nAdvarsel (kan få udgivelsen til at fejle):\n- " + strings.Join(res.Warnings, "\n- ")
	}
	return msg
}

// deployStatus er første del af deployMessage.
func (s *Server) deployStatus(r *http.Request, res *DeployResult) string {
	url := s.appURL(res.App.Slug)
	if res.NeedsOK {
		return fmt.Sprintf("Version %d er modtaget, men «%s» er ny og skal først godkendes af ejeren. Bed ejeren åbne dashboardet (%s) og trykke «Godkend og udgiv». Adressen bliver %s. Brug get_status for at se, når den er live.",
			res.VersionN, res.App.Name, s.dashboardURL(r), url)
	}
	return fmt.Sprintf("Version %d bygges nu. Adressen er %s. Brug get_status for at følge med.", res.VersionN, url)
}

// upload modtager en pakket mappe på en engangsadresse fra prepare_upload.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	tok := r.PathValue("token")
	var up *Upload
	var user *User
	_ = s.st.Do(func(db *DB) error {
		up = db.Uploads[tok]
		delete(db.Uploads, tok) // kan kun bruges én gang
		if up != nil {
			if u := db.user(up.UserID); u != nil {
				cp := *u
				user = &cp
			}
		}
		return nil
	})
	if up == nil || time.Now().After(up.Expires) {
		http.Error(w, "Upload-adressen er ugyldig, udløbet eller allerede brugt. Bed om en ny med prepare_upload.", http.StatusNotFound)
		return
	}
	if user == nil || !canBuild(user) {
		http.Error(w, "Kontoen må ikke udgive apps.", http.StatusForbidden)
		return
	}
	if existing, found := s.mgr.get(slugify(up.Name)); found && !canManage(&existing, user) {
		http.Error(w, "Navnet er optaget af en anden app.", http.StatusConflict)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUpload)
	res, err := s.mgr.DeployArchive(up.Name, up.Port, up.Note, user.ID, body)
	if err != nil {
		code := http.StatusBadRequest
		if err == errTooLarge {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "Fejl: "+err.Error(), code)
		return
	}
	fmt.Fprintln(w, s.deployMessage(r, res))
}
