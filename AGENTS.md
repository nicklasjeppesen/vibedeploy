# AGENTS.md: Vibe Deploy for AI-agenter

Læs denne fil, før du ændrer noget. Den er kort med vilje; detaljerne står i `ARCHITECTURE.md` (hvordan koden
virker), `README.md` (hvad den kan) og `DEPLOY.md` (server, releases, signering).

## Hvad projektet er

Én Go-binær, der lader en AI (via MCP) udgive apps som Docker-containere på ejerens egen server, mens ejeren styrer
dem i et dashboard. Målgruppen er ikke-tekniske folk, så alt i dashboardet og alle fejlbeskeder skal kunne forstås
uden IT-viden. Én ejer pr. server, som inviterer familie eller kolleger. Prototype, men tænkt til rigtig brug.

## Kør og test

```sh
go run . -runtime demo        # uden Docker; åbn http://localhost:8080
go run . -runtime docker      # rigtige containere
go test ./...                 # hurtigt; kør også med -race, når du rører låse eller goroutines
gofmt -l .                    # skal være tom
```

- Demo-runtime'n (`demoRT` i `runtime.go`) er en lille HTTP-server i samme proces. Den bruges i testene og er den
  hurtigste måde at prøve et flow på. Docker-delen er ikke dækket af tests: sig det, hvis du ændrer den.
- `go.mod` kræver Go 1.26. Den eneste afhængighed er `modernc.org/sqlite` (ren Go, så der krydskompileres uden cgo).

## Faste valg (bryd dem ikke uden at spørge)

- **Kun standardbiblioteket** plus `modernc.org/sqlite`. Ingen web-frameworks, ingen ORM, ingen nye afhængigheder.
- **Dansk** i UI, fejlbeskeder, logbeskeder og kommentarer. Hver funktion har en dansk doc-kommentar, der siger,
  hvad den gør og hvorfor. Korte sætninger, almindelige ord, ingen emoji.
- **AI'en kan aldrig læse hemmeligheder.** Den kan kun bede om en tom plads (`request_secret`), som ejeren udfylder.
  `set_env` afviser navne, der ligner hemmeligheder (`secretish`). Hemmeligheder må aldrig stå i logs eller MCP-svar.
- **En ny app går først live, når ejeren har godkendt den.** Senere versioner bygges automatisk.
- **AI'en handler som sin bruger** og kan kun se og ændre den brugers egne apps (`canManage`, `canBuild`).
- **Én container og én port pr. app.** Ingen docker-compose. Data, der skal overleve, ligger i `/data`.
- **Containere publiceres kun på `127.0.0.1`.** Kun Caddy er åben udadtil.
- **Skabeloner, CSS og JS er indlejret** (`go:embed`), så binæren kan køre alene.

## Ting, der er nemme at ødelægge

- **Udrulning uden nedetid** (`startVersion` i `deploy.go`): hver app har to pladser, `vd-<slug>` og `vd-<slug>_b`
  (`instName`). `App.Slot` er den aktive, `App.HostPort` er porten, proxyen sender til. En ny version startes på den
  ledige plads; først når den svarer på `/health`, skiftes `HostPort`/`Slot` i ét skridt. Den gamle lukkes af
  `retire` efter `drainDelay`. Stop aldrig den aktive container før skiftet.
- **To låse pr. app:** `lockFor(slug)` holdes under hele en bygning; `slotLock(slug)` dækker start → skift og
  `retire`. `retire` må ikke tage `lockFor`, ellers kører to containere under hele den næste bygning.
- **Slugs må ikke indeholde `_` eller `#`.** `_` adskiller pladsen i containernavne (`appOf`), `#` bruges i
  låsenøgler, og `backups/_server` kan derfor ikke forveksles med en app.
- **`Runtime.Start/Stop/Logs` tager et containernavn, ikke en slug.** Brug `m.stopAll(slug)` for at stoppe en app.
- **Proxyen sender trafik videre, mens en ny version bygges** (`serving` i `web.go`). Vis ikke en ventende side,
  hvis den forrige version kører.
- **`Reconcile` overtager kørende containere** ved opstart i stedet for at genstarte dem. Det er det, der gør
  opdateringer af Vibe Deploy selv usynlige for appene.
- **Lagring:** al tilstand er JSON pr. række i `state.db` (tabeller i `tables` i `store.go`). Nye felter er fine;
  ældre versioner ignorerer dem. Ændrer du data, så en ældre version ikke kan læse dem rigtigt, så hæv
  `schemaVersion`. En ny tabel skal også med i `rows()` og `put()`.
- **Opdatering af Vibe Deploy selv** (`update.go`, `release.go`): serveren (bruger `vibedeploy`) må kun lægge filen
  `update-request`; root-delen (`vibedeploy update`) henter selv den nyeste signerede release. Root-delen må kun røre
  faste filnavne direkte i data-mappen og kun med `rename`/`remove`/`writeFileAtomic`, som ikke følger symlinks.
  Lad den aldrig læse en sti, et filnavn eller en version fra en fil, serveren har skrevet.
- **Versioner** sættes med `-ldflags "-X main.version=vX.Y.Z"`. `dev` opdaterer sig aldrig. Tags skal være `vX.Y.Z`.

## Hemmelige filer: commit dem aldrig

| Fil | Hvad | Hvor den hører hjemme |
|---|---|---|
| `release-signing.key` | Signerer releases | Ejerens Mac og GitHub-secret `VIBEDEPLOY_SIGNING_KEY` |
| `secret.key`, `data/` | Krypterer appenes hemmeligheder, lokale data | Kun på maskinen, der kører serveren |

`.gitignore` udelukker `*.key` og `/data/`. Lav aldrig en signeringsnøgle på vegne af ejeren; han skal selv køre
`go run . keygen`, så den hemmelige del aldrig forlader hans maskine. `releasePublicKey` i `releasekey.go` er
offentlig og skal committes.

## Når du ændrer noget

1. Skriv eller ret en test. Til flows: brug `newTestManager` og demo-runtime'n (se `zerodowntime_test.go` og
   `update_test.go` for mønstre, fx en runtime, der fejler med vilje, eller en klient, der sender forespørgsler
   hele tiden).
2. Hold `ARCHITECTURE.md` opdateret, når opbygningen ændrer sig, `README.md`, når en funktion kommer til eller
   ændres, og `DEPLOY.md`, når noget på serveren ændres. Opdatér «Kendt begrænset» i `README.md`, hvis noget ikke
   er afprøvet.
3. Ændrer du byggereglerne for apps, så ret også `guideRules` i `buildguide.go`; det er dem, AI'en får.
4. Commit kun, når ejeren beder om det.

## Kort over filerne

| Fil | Indhold |
|---|---|
| `main.go` | Flag, underkommandoer (`install`, `update`, `version`, `keygen`, `sign`), opstart, nedlukning |
| `deploy.go` | `Manager`: udgivelse, bygge-pipeline, udrulning uden nedetid, rollback, variabler, `Reconcile` |
| `runtime.go` | `Runtime`-interfacet, Docker og demo, `instName`/`otherSlot`/`appOf`, `waitHealthy` |
| `web.go` | Ruter, proxy til apps (`proxyApp`, `serving`), dashboard-handlere |
| `mcp.go` | MCP-endpoint og de 13 værktøjer (`callTool`) |
| `store.go` | Datamodel (`DB`, `App`, `Settings`), SQLite-lagring, `schemaVersion` |
| `update.go`, `schedule.go`, `release.go`, `version.go`, `releasekey.go` | Selvopdatering og dens tidspunkt (valgt tidszone), signerede releases, versioner |
| `access.go`, `auth.go`, `oauth.go` | Roller og deling, login og CSRF, OAuth 2.1 til AI-værktøjer |
| `data.go`, `gitdeploy.go`, `imagedeploy.go` | Upload, `/data`, backups; git og deploy keys; færdige images |
| `metrics*.go`, `serverpage.go`, `setup.go` | Siden Server, statussiden under installationen |
| `install.go`, `install.sh`, `deploy/` | Installation, systemd-enheder, Caddyfile, cloud-init |
| `web/` | HTML-skabeloner, CSS, JS |
