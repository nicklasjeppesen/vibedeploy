# Sådan virker Vibe Deploy

`README.md` beskriver, hvordan man bruger Vibe Deploy, og `DEPLOY.md`, hvordan man sætter den op på en server.
Denne fil beskriver, hvordan koden hænger sammen: hvad der sker, fra en AI kalder et værktøj, til appen kører
på `https://navn.dit-domæne.dk`.

## Overblik

Vibe Deploy er ét Go-program (standardbiblioteket plus `modernc.org/sqlite`), der har fire roller:

1. **Dashboard** til mennesker: konti, apps, godkendelser, hemmeligheder, deling (`web.go`, `access.go`, `web/`).
2. **MCP-server** til AI'er: `/mcp` med 13 værktøjer (`mcp.go`), beskyttet af **OAuth 2.1** (`oauth.go`).
3. **App-styring**: bygger, starter, stopper og ruller apps tilbage (`deploy.go`, `data.go`, `gitdeploy.go`, `runtime.go`).
4. **Reverse proxy** for apps: `<slug>.<domæne>` sendes videre til appens container (`web.go: proxyApp`).

```
          AI-værktøj (Claude)                     Browser
                 │  MCP (JSON-RPC over HTTP)         │  dashboard / app-adresse
                 ▼                                   ▼
        ┌──────────────────────── Caddy (https, kun på en rigtig server) ────────┐
        └──────────────────────────────────┬─────────────────────────────────────┘
                                           ▼
   ┌──────────────────────────── Vibe Deploy (127.0.0.1:8080) ─────────────────────────────┐
   │  handler(): Host = <slug>.<domæne>?  ── ja ──▶ proxyApp ──▶ 127.0.0.1:<HostPort>       │
   │                                      └─ nej ─▶ dashboard · OAuth · /mcp · /upload      │
   │                                                     │                                  │
   │  Server ──▶ Manager (udgivelse, versioner, data) ──▶ Runtime (docker / demo)           │
   │       └───▶ Store (state.db, én lås)                                                   │
   └────────────────────────────────────────────────────────────────────────────────────────┘
```

## Filerne

| Fil | Indhold |
|---|---|
| `main.go` | Flag, opstart, valg af runtime, `Server`-strukturen, indlæsning af skabeloner |
| `store.go` | Datamodellen (`DB`, `User`, `App`, `Version` …) og `Store`, der gemmer alt i SQLite (`state.db`) |
| `crypto.go` | Kryptering af hemmeligheder (AES-256-GCM) og indlæsning af nøglen |
| `buildguide.go` | Byggevejledningen til AI'en (`get_build_guide`) og tjek af Dockerfile |
| `metrics.go`, `metrics_*.go` | `Monitor`: måler CPU, hukommelse, disk og hver apps forbrug (Linux via /proc, macOS via sysctl) |
| `serverpage.go` | Siden «Server» med nøgletal, SVG-grafer og forbrug pr. app |
| `install.go` | `vibedeploy install`: bruger, nøgle, opsætningskode, systemd, opdateringsenheder og Caddyfile (skabeloner fra `deploy/`, indlejret) |
| `update.go` | Opdatering af Vibe Deploy selv: `Updater` (tjek, knap, nat) og `vibedeploy update` (root-delen, tilbagerulning), `/healthz` |
| `release.go`, `releasekey.go` | Signerede releases: `release.json` + ed25519-signatur, `keygen`, `sign`, verifikation og hentning |
| `version.go` | `version` (sættes med `-ldflags`) og sammenligning af versioner |
| `setup.go` | Statussiden på `http://<IP>` under installationen (DNS og https) |
| `auth.go` | Adgangskoder (PBKDF2), dashboard-sessioner, `requireUser`, CSRF-tjek |
| `access.go` | Roller og synlighed (`canBuild`, `canManage`, `canOpen`), login foran apps, personer og invitationer |
| `oauth.go` | OAuth 2.1 med PKCE og dynamisk klientregistrering, så AI-værktøjer kan logge ind |
| `mcp.go` | MCP-endpointet, værktøjsdefinitioner og `callTool`, plus modtagelse af uploads |
| `deploy.go` | `Manager`: udgivelse, bygge-pipeline, genstart, rollback, variabler, hemmeligheder |
| `data.go` | Udpakning af uploads, `/data`-mapper, backups, gendannelse og oprydning |
| `gitdeploy.go` | Hentning fra git og deploy keys |
| `gitnew.go` | Formularen «Ny app fra git» i dashboardet (`/apps/new/git`), bruger `DeployFromGit` |
| `runtime.go` | `Runtime`-interfacet og de to udgaver: Docker og demo |
| `web.go` | Ruter, proxy til apps, dashboard-sider og knapperne på appens side |
| `web/` | HTML-skabeloner, CSS og JS (indlejret i binæren med `go:embed`) |
| `deploy/` | Caddyfile, systemd-tjenesten og de to enheder til opdatering (`vibedeploy-update.service`/`.path`) |

## Opstart (`main`)

0. Underkommandoer: `install`, `update`, `version`, `keygen`, `sign` (se nedenfor og `DEPLOY.md`).
1. Læser flag (`-addr`, `-domain`, `-data`, `-runtime`, `-public-url`).
2. Læser nøglen til hemmeligheder (`keyPath`), åbner `data/state.db` med `OpenStore` (flytter en gammel `state.json` over) og migrerer gamle data (første bruger bliver ejer, apps uden synlighed bliver private). Er data skrevet med et nyere skema (`Settings.Schema` > `schemaVersion`), nægter den at starte.
3. Vælger runtime: `docker` (tjekker, at Docker kører) eller `demo`.
4. Er der ingen konti, og kører serveren ikke på `localhost`, udskrives en **opsætningskode**, som skal bruges til den første konto.
5. `Manager.Reconcile` overtager apps, hvis container stadig svarer (uden genstart), starter resten igen og markerer afbrudte bygninger som fejlede.
6. `DailyBackups`, `Monitor` og `Updater` startes i baggrunden, og HTTP-serveren begynder at lytte. SIGTERM og Ctrl+C lukker pænt ned (højst 10 sekunder); appenes containere kører videre.

## Tilstand og lagring (`store.go`)

Al tilstand ligger i én struktur, `DB`, i hukommelsen. Den gemmes i SQLite (`state.db`, WAL, rettigheder 0600):
én tabel pr. samling (`users`, `apps`, `sessions` …), hver med `id`, `pos` (rækkefølge) og `data` (JSON).
Efter hver ændring sammenlignes rækkerne med det, der sidst blev gemt, og kun ændrede rækker skrives (eller slettes)
i én transaktion. Hemmeligheder i `App.Secrets` krypteres med AES-256-GCM, når rækken skrives, og bindes til
app og navn (`secretAD`), så en krypteret værdi ikke kan flyttes til en anden app. Nøglen ligger uden for databasen.
`Store` har én lås og tre måder at bruge den på:

- `View(f)`: læs under lås uden at kopiere (hurtige opslag).
- `Do(f)`: ændr under lås, og gem bagefter, hvis `f` ikke fejler (én SQLite-transaktion).
- `Snapshot()`: en dyb kopi, som kan læses uden lås (bruges til at vise sider).

`Manager.get(slug)` returnerer på samme måde en kopi af én app, og `Manager.update(slug, f)` ændrer den under lås.

Filer på disken (under `-data`):

```
data/
├── state.db(-wal, -shm)        konti, sessioner, tokens, apps, invitationer … (SQLite)
├── secret.key                  nøglen til hemmeligheder, hvis ingen anden er angivet (se DEPLOY.md)
├── apps/<slug>/v<N>/           kildekoden til hver version (de 5 nyeste + den, der kører)
├── volumes/<slug>/             appens /data – overlever genstart, nye versioner og rollback
├── backups/<slug>/*.tar.gz     backups af /data (de 10 nyeste)
├── logs/<slug>.log(.1)         bygge- og deploylog, roteres ved 2 MB
├── keys/<slug>_ed25519(.pub)   deploy keys til private git-repos, plus known_hosts
└── run/<slug>.env              midlertidig env-fil til docker run (slettes straks)
```

## Tre slags login

| Hvem | Hvordan | Kode |
|---|---|---|
| Menneske i dashboardet | Cookie `vd_session` (14 dage), en `Session` uden `AppSlug` | `startSession`, `currentUser`, `requireUser` |
| Menneske på en apps adresse | Cookie `vd_app` (7 dage), kun gyldig for den ene app | `appLogin`, `appAuth`, `appSessionOK` |
| AI-værktøj på `/mcp` | `Authorization: Bearer vdt_…` (OAuth-token i 90 dage, eller adgangsnøgle i 1 år) | `oauth.go`, `mcpAuth` |

### Login på en apps adresse

Apps kører på deres egen adresse, så dashboardets cookie kan ikke bruges der. I stedet bruges en engangsbillet:

```
Browser ─GET navn.domæne/side──▶ proxyApp: ingen gyldig vd_app-cookie
        ◀─303 til dashboard/app-login?app=navn&next=/side
        ─GET /app-login──▶ requireUser (logger ind, hvis nødvendigt) → canOpen? → Ticket (60 sek.)
        ◀─303 til navn.domæne/_vd/auth?t=<billet>&next=/side
        ─GET /_vd/auth──▶ appAuth: billetten bruges op → ny app-session → cookie vd_app
        ◀─303 til /side ──▶ proxyApp sender videre til appen (uden vd_app-cookien)
```

Offentlige apps (`VisPublic`) springer det hele over.

### AI'en logger ind (OAuth 2.1 + PKCE)

1. AI'en kalder `/mcp` uden token og får `401` med en henvisning til `/.well-known/oauth-protected-resource`.
2. Den læser `/.well-known/oauth-authorization-server` og registrerer sig selv via `POST /register` (RFC 7591).
3. Brugeren sendes til `/authorize`, logger ind og trykker **Tillad** på samtykkesiden (`authorizePage`, `authorizeSubmit`).
4. AI'en bytter koden til et token på `/token`. Koden kan kun bruges én gang og skal passe til PKCE-challengen.
5. Herefter sender den `Authorization: Bearer <token>` med hvert kald til `/mcp`.

Kun brugere med bygge-ret (`canBuild`) kan forbinde en AI, og `mcpAuth` tjekker retten ved hvert kald.

## Roller og adgang (`access.go`)

- **Ejer** (`RoleOwner`): den første konto. Kan se og styre alt og invitere andre.
- **Medlem** (`RoleMember`): kommer ind via invitation. Med `Builder` må vedkommende forbinde en AI og lave egne apps.
- Hver app har en **ejer** (`OwnerID`) og en **synlighed**: `private`, `shared` (udvalgte i `Access`), `members` eller `public`.
- `canManage(app, bruger)`: må styre appen (ejeren af appen eller serverens ejer).
- `canOpen(app, bruger)`: må åbne appen (ud fra synligheden).
- En AI handler som sin bruger: `callTool` lader kun den bruger se og ændre apps, som vedkommende må styre.

## Udgivelse af en app

Der er tre veje ind, og de ender alle i `Manager.deployWith`:

| Værktøj | Vej | Kode |
|---|---|---|
| `deploy_app` | Filerne som tekst i værktøjskaldet (højst 300 filer / 8 MB) | `Manager.Deploy` → `writeFiles` |
| `prepare_upload` | Engangsadresse; AI'en kører `tar … \| curl -X PUT` | `upload` → `DeployArchive` → `extractTarGz` |
| `deploy_from_git` | Serveren kloner selv repoet | `DeployFromGit` → `gitClone` → `cleanClone` |
| Dashboard: Ny app fra git | Samme, fra en formular | `gitNewSubmit` → `DeployFromGit` |
| `deploy_image` | Serveren henter et færdigt image (Docker Hub, ghcr.io …) | `DeployImage` → `parseImage`; `pipeline` kalder `Runtime.Pull` i stedet for `Build` |

`deployWith` gør følgende:

1. Laver navnet om til en slug (`slugify`) og tjekker, at den er gyldig og ikke reserveret.
2. Lægger filerne i `apps/<slug>/v<N>/` og kræver en `Dockerfile` (med Docker-runtime).
3. Opretter appen, hvis den er ny (privat, med en ledig port på værten), og tilføjer `Version N`.
4. **Ny app** → status `waiting`: ejeren skal trykke «Godkend og udgiv» (`Approve`).
   **Godkendt app** → status `building`, og `pipeline` startes i baggrunden med det samme.

### Bygge-pipelinen (`Manager.pipeline`)

Kører med appens egen lås, så to bygninger af samme app aldrig kører samtidig.

```
waiting ──Approve──▶ building ──Build ok──▶ (backup af /data) ──▶ starting ──svarer på HTTP──▶ live
                        │                                            │                           │
                        └─────── fejl ─────────┬─────────────────────┘                    pruneVersions
                                               ▼
                                 Kører den forrige version stadig?
                                   ja ──▶ live, uden afbrydelse («version N fejlede, N-1 kører stadig»)
                                   nej, men der er en ─▶ start den igen → live
                                   ingen ─▶ failed (med forklaring fra explain())
```

Den forrige version kører hele vejen igennem: proxyen sender trafik til den, mens den nye bygges og starter
(`serving`: status `live`, eller `building`/`starting` med en kørende version).

- `Runtime.Build` bygger imaget (`docker build`, højst 10 minutter). Har versionen `From` sat (deploy_image),
  kalder pipelinen i stedet `Runtime.Pull`: `docker pull` med en midlertidig, tom `DOCKER_CONFIG`, der kun indeholder
  appens login (`DOCKER_BRUGERNAVN`/`DOCKER_TOKEN`), og derefter `docker tag` til `vd-<slug>:v<N>`. Login sendes ikke ind i containeren (`envFor`).
- `startVersion` udruller uden nedetid («blue-green»). Hver app har to pladser, `vd-<slug>` og `vd-<slug>_b`
  (`instName`); `App.Slot` er den aktive. Den nye container startes på den ledige plads og en ny port med
  `envFor(app)` (PORT, DATA_DIR, variabler, udfyldte hemmeligheder), og serveren venter op til 40 sekunder på, at
  den svarer på `/health` (`waitHealthy`). Ethvert svar under 500 tæller, så apps uden `/health` (404) også godkendes;
  en app kan svare 503, indtil den er klar. Først da skiftes `HostPort` og `Slot` i ét skridt; proxyen læser
  `HostPort` ved hver forespørgsel. Den gamle container lukkes efter `drainDelay` (10 sekunder) med SIGTERM
  (`retire`), så forespørgsler i gang kan blive færdige. Svarer den nye ikke, fjernes den, og den gamle kører videre.
- Pladslåsen (`slotLock`) dækker start → skift og `retire`, så en forsinket `retire` aldrig lukker en container,
  som en ny udrulning lige har startet på samme plads. Appens lås (`lockFor`) holdes under hele bygningen og kan
  ikke bruges til det.
- Under overlappet deler de to containere `/data`. Byggevejledningen beder derfor om SQLite med WAL.
- `pruneVersions` sletter filer og images fra gamle versioner (de 5 nyeste og den, der kører, beholdes).

### Andre handlinger

- `Restart` / `Rollback` → `switchTo`: starter en allerede bygget version i baggrunden, også uden nedetid.
- `Stop`: fjerner begge pladsers containere (`stopAll`); status `stopped`.
- `Restore` (gendan backup af data): stopper appen kort, så data ikke ændres under den, og starter den igen.
- `Delete`: fjerner container, versioner, data, backups, log og deploy key.
- `Reconcile` (ved opstart): overtager apps, hvis container på `HostPort` svarer, og rydder kun den ubrugte plads op.
  Svarer den ikke (fx efter en genstart af maskinen eller med demo-runtime'n), startes den nuværende version igen.

## Docker og demo (`runtime.go`)

Alt, der rører containere, går gennem interfacet `Runtime` (`Build`, `Pull`, `Start`, `Stop`, `Logs`, `RemoveImage`).
`Start`, `Stop` og `Logs` arbejder på én container (en plads), ikke på en hel app.

- **`dockerRT`**: containeren hedder `vd-<slug>` eller `vd-<slug>_b`, får højst 512 MB og 1 CPU, `--restart unless-stopped`,
  `/data` monteret fra `volumes/<slug>`, og porten publiceres kun på `127.0.0.1:<port>`. `Stop` giver containeren
  10 sekunder til at lukke pænt (`docker stop -t 10`) og fjerner den. `docker stats` lægger de to pladser sammen.
  Miljøvariabler sendes via en midlertidig fil (0600), så de ikke kan ses i procesoversigten.
- **`demoRT`**: ingen Docker. «Bygningen» er en kort pause, og appen er en lille HTTP-server i samme proces,
  der serverer `index.html` fra appens mappe. Bruges til at prøve flowet og i testene.

## Variabler og hemmeligheder

- **Variabler** (`Env`): sættes af AI'en med `set_env` (`MergeEnv`) eller af ejeren i dashboardet (`SetEnv`).
  `MergeEnv` afviser navne, der ligner hemmeligheder (`secretish`).
- **Hemmeligheder** (`Secrets`): hver har et variabelnavn (`Key`, fx `DB_PASSWORD` eller `SMTP_PASSWORD`),
  og appen læser værdien som en miljøvariabel med det navn. AI'en kan kun bede om en tom plads
  (`request_secret` → `RequestSecrets`). Ejeren udfylder værdien i dashboardet (`SetSecret`) eller tilføjer selv
  en ny hemmelighed med navn og værdi i ét trin (`appAction` «newsecret»). Ingen værktøjer returnerer værdierne;
  `get_status` nævner kun, hvilke der mangler.
- Ændringer sætter `NeedsRestart`. `set_env` fra AI'en genstarter appen selv.

## Data og backups (`data.go`)

- Hver app får `volumes/<slug>`, monteret på `/data` (stien står også i `DATA_DIR`). Alt andet i containeren
  forsvinder ved genstart og ny version.
- `Backup` pakker mappen til `backups/<slug>/ÅÅÅÅMMDD-TTMMSS.tar.gz`: én gang i døgnet (`DailyBackups`),
  før hver ny version og når ejeren beder om det. De 10 nyeste beholdes.
- `Restore` stopper appen, tager en backup af den nuværende data, pakker den valgte backup ud og
  starter appen igen. Fejler udpakningen, lægges den gamle data tilbage.

## Hentning fra git (`gitdeploy.go`)

1. `parseRepo` accepterer kun `ejer/repo` (GitHub), `https://`, `ssh://git@` og `git@vært:sti`.
   Lokale stier og andre protokoller afvises.
2. Først prøves en anonym `git clone --depth 1` over https (offentlige repos).
3. Fejler det, laves en deploy key til appen (`ensureKey`, ed25519). Første gang returneres `ErrNeedKey`,
   og AI'en får den offentlige nøgle og et link til, hvor ejeren tilføjer den (`needKeyText`).
4. Næste gang klones over ssh med nøglen. Nøglen er bundet til ejer og repo og forlader aldrig serveren.
5. `cleanClone` fjerner `.git` og symlinks og håndhæver de samme størrelsesgrænser som upload.

## Routing og sikkerhedsheaders (`web.go: handler`)

- `Host` = `<slug>.<domæne>` → `proxyApp`. Alt andet går til ruterne for dashboard, OAuth, MCP og upload.
- Dashboard-svar får `X-Frame-Options: DENY`, `nosniff`, en stram `Content-Security-Policy` og `Referrer-Policy`.
- POST-formularer i dashboardet tjekkes med `sameOrigin` (Origin/Referer) oven på `SameSite=Lax`-cookies.
- `/internal/tls-ask` bruges af Caddy til on-demand TLS: der udstedes kun certifikater til hoveddomænet og
  til apps, der findes. Kun kald direkte fra samme maskine accepteres.

## Opdatering af Vibe Deploy (`update.go`, `release.go`)

```
Server (bruger vibedeploy)                         systemd / root
─────────────────────────                          ─────────────────────────────────────────────
Updater.Run: hvert minut tick; hver time fetchRelease ──▶ «Ny version» på siden Server
«Opdater nu» / ejerens tidspunkt med automatik
  └─ Request: VACUUM INTO update-state-backup.db
              + update-request ───────────────────▶ vibedeploy-update.path ser filen
                                                    vibedeploy update:
                                                      fetchRelease (signatur) → downloadVerified (SHA-256)
                                                      «ny version» skal svare sin egen version
                                                      stop → vibedeploy.prev (hård kobling) → ny på plads → start
                                                      /healthz med ny version inden for 60 s?
                                                        ja ─▶ status ok
                                                        nej ─▶ gammel binær tilbage → start
                                                               starter den ikke: backup af state.db tilbage → start
                                                      update-status.json ──▶ vises på siden Server
```

- **Tillid:** `release.json` (version, link, SHA-256 pr. binær) signeres med ed25519 (`signContext` foran). Den offentlige
  nøgle står i `releasekey.go`. Versionen er en del af det signerede, og der installeres kun en nyere version (`newer`),
  så ingen kan sende serveren tilbage til en ældre. En udviklingsudgave (`version = "dev"`) opdaterer sig aldrig.
- **Rettigheder:** serveren kan kun lægge en fil, der beder om «nyeste signerede release». Root-delen læser ikke filens
  indhold, rører kun faste navne direkte i data-mappen og bruger `rename`/`remove`/`O_EXCL` (`writeFileAtomic`), som
  ikke følger symlinks.
- **Data:** `schemaVersion` hæves, når en version ændrer data, så en ældre ikke kan læse dem. Ved tilbagerulning prøves
  den gamle version først på de nuværende data; kun hvis den ikke starter, lægges backuppen tilbage, og de nyere data
  gemmes som `state.db.efter-opdatering`.
- **Tidspunkt** (`schedule.go`): ejeren vælger dag (`daily` eller en ugedag), «HH:MM» og en IANA-tidszone
  (standard `Europe/Copenhagen`; `updateTZs` er valgene i dropdown'en med danske navne og nuværende UTC-forskel,
  «Andet …» giver et felt til enhver anden zone (`tzFromForm`), `Local` afvises), gemt i `Settings`. Det regnes i den
  tidszone (tidszonerne er indlejret med `time/tzdata`), ikke serverens ur. `tick` kører hvert minut;
  er tidspunktet nået (og højst to timer siden, `updateWindow`), og er der ikke prøvet siden (`LastAutoTry`), gemmes
  forsøget, og opdateringen bedes om. Siden viser næste mulige tidspunkt (`schedule.next`).
- **Under genstarten:** app-containerne kører videre, `Reconcile` overtager dem, og Caddy holder forespørgsler i op til
  15 sekunder (`lb_try_duration`) i stedet for at svare 502.

## Tests

```sh
go test ./...
```

Testene (`*_test.go`) er enhedstests af de dele, der er nemmest at få forkert: PBKDF2, `slugify`, `checkPath`,
`secretish`, adgangsreglerne, `stripCookie`, udpakning af uploads (inkl. ondsindede stier), `parseRepo`,
`checkBranch`, og at alle sider kan tegnes. `zerodowntime_test.go` kører udrulning, genstart, rollback og en fejlet
version med demo-runtime'n, mens en klient sender forespørgsler hele tiden, og kræver, at ingen af dem fejler.
`update_test.go` dækker versioner, signatur (forkert nøgle, ændret manifest), SHA-256, `vibedeploy update` (ok, intet
nyt, forkert binær, tilbagerulning med og uden backup af data), anmodning og automatik på det valgte tidspunkt (i valgt tidszone og ved sommertid), skemavagten,
`Reconcile`, der overtager kørende apps, `/healthz` og opdateringsenhederne. Docker-delen er ikke dækket af tests.
