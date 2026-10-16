# Vibe Deploy (prototype)

En lille Go-server, der lader en AI (fx Claude) udgive apps som containere på din egen maskine,
mens du styrer dem i et dashboard. Go's standardbibliotek plus én afhængighed: `modernc.org/sqlite` (SQLite i ren Go, så der stadig kan krydskompileres uden cgo).

## Kør den

```sh
go run . -runtime demo      # uden Docker: appene spilles af serveren selv
go run . -runtime docker    # rigtige containere (kræver Docker)
```

Åbn http://localhost:8080, opret din konto, og følg trin 2 og 3 (MCP-adresse og godkendelse).
Apps får adressen `http://<navn>.localhost:8080`. Chrome og Firefox finder selv `*.localhost`;
hvis din browser ikke gør, så brug `curl --resolve` eller en linje i hosts-filen.

Flag: `-addr`, `-domain`, `-data`, `-runtime`, `-public-url`.

## Forbind din AI

**Claude Code (virker direkte mod localhost):**

```sh
claude mcp add --transport http min-server http://localhost:8080/mcp
```

Kør derefter `/mcp` i Claude Code og vælg «Authenticate». Du sendes til dashboardet og trykker Tillad.
Alternativt: opret en adgangsnøgle under «Forbind AI», og tilføj den med `--header "Authorization: Bearer …"`.

**claude.ai og andre webtjenester** kalder din server fra internettet og kræver en offentlig https-adresse.
For at efterligne en rigtig server kan du bruge en tunnel:

```sh
cloudflared tunnel --url http://localhost:8080          # eller ngrok http 8080
go run . -runtime demo -public-url https://DIN-TUNNEL-ADRESSE
```

Apps er stadig på `*.localhost`, fordi en gratis tunnel kun giver ét værtsnavn.
På en rigtig server med wildcard-DNS (`*.dit-domæne.dk`) bruger du `-domain dit-domæne.dk` bag Caddy/https.

## Udgivelse, data og oprydning

**Fire måder at sende en app på:**
- `deploy_app`: filerne som tekst i selve værktøjskaldet. Til små apps, fx lavet i claude.ai. Højst 300 filer og 8 MB, kun tekst.
- `prepare_upload`: giver en engangsadresse (15 minutter) og en kommando, der pakker mappen og sender den med `curl`.
  Kræver en terminal (fx Claude Code). Op til 200 MB pakket og 500 MB udpakket, billeder og andre binære filer er med,
  og det koster næsten ingen tokens. `node_modules`, `.git` og `.env` sendes ikke med; symlinks springes over.

- `deploy_from_git`: serveren henter selv koden fra et git-repo (GitHub, GitLab m.fl.). Virker fra alle AI-værktøjer,
  også claude.ai uden terminal, og har samme grænser som upload. Offentlige repos hentes direkte. Private repos:
  første kald svarer med en **deploy key**, som ejeren tilføjer i repoets indstillinger (kun læseadgang), og så kører
  man værktøjet igen. Hver app har sin egen nøgle, bundet til ejeren og repoet; den hemmelige del forlader aldrig serveren.
  Under appens Indstillinger kan man hente nyeste version fra git med ét klik eller lave en ny nøgle.
  Kræver `git` og `ssh` på serveren (`apt install git openssh-client`).

- `deploy_image`: serveren henter et færdigt image fra Docker Hub, GitHub (`ghcr.io`) eller et andet registry,
  fx `nginx:1.27` eller `ghcr.io/ejer/app:latest`. Intet bygges, og der skal ingen Dockerfile til. Er imaget privat,
  opretter værktøjet felterne `DOCKER_BRUGERNAVN` og `DOCKER_TOKEN` under appens hemmeligheder, og ejeren indsætter
  sit login dér (en token med kun læseadgang). Login bruges kun til at hente imaget: det sendes ikke ind i containeren,
  og serverens egne Docker-logins bruges aldrig. Under Indstillinger kan man hente nyeste udgave af samme tag med ét klik.
  Imaget skal være bygget til serverens processortype (typisk `linux/amd64`).

**Data, der overlever:** Hver app får en mappe monteret på `/data` (stien står også i `DATA_DIR`).
Den overlever genstarter, nye versioner og rollback. Alt andet i containeren starter forfra.
Der tages backup af `/data` én gang i døgnet og før hver ny version; de seneste 10 gemmes.
Under fanen **Data** kan ejeren tage en backup og gendanne en tidligere. Ved gendannelse gemmes den nuværende data først.

**Oprydning:** De 5 nyeste versioner (og den, der kører) beholder filer og image til rollback. Ældre ryddes op.
Logfiler roteres ved 2 MB.

## Server, lager og hemmeligheder

**Siden «Server»** (kun ejeren) viser CPU, hukommelse og disk lige nu og det seneste døgn, hvad hver app bruger
(CPU og hukommelse med Docker, plads på disken altid), og hvad der fylder: appenes data, backups, versioner, logfiler,
databasen og Dockers images. Er disken eller hukommelsen over 90 %, står der en advarsel på forsiden.
Appens egen side viser dens forbrug under Oversigt.

**Lager:** Al tilstand ligger i SQLite (`data/state.db`). Findes der en `state.json` fra en tidligere version, flyttes den
automatisk over ved første start og omdøbes til `state.json.importeret` (slet den bagefter: den har hemmeligheder i klartekst).
Der tages en kopi af databasen hver nat i `backups/_server/` (de 7 nyeste).

**Hemmeligheder** krypteres med AES-256-GCM, før de gemmes. Nøglen ligger uden for databasen: `-key-file`, ellers
systemd's `LoadCredential=secret.key:…`, ellers `data/secret.key` (oprettes automatisk; fint lokalt, men på en server
bør den ligge uden for data-mappen, se `DEPLOY.md`).

**Byggevejledning til AI'en:** `get_build_guide` giver reglerne (Dockerfile, lyt på 0.0.0.0 og `PORT`, data i `DATA_DIR`,
en `/health`-side) og en færdig skabelon til `static`, `node`, `python` eller `go`. Ved udgivelse tjekkes Dockerfile for
almindelige fejl (EXPOSE med en anden port, en app der kun lytter på localhost), og AI'en får en advarsel med i svaret.
Ved opstart venter serveren på `/health` (et svar under 500 tæller, så apps uden siden virker som før).

## Personer og adgang

Serveren har **én ejer**, som inviterer andre (familie, kolleger):

- Den første konto bliver ejer. Under **Personer** opretter ejeren et invitationslink (virker én gang i 7 dage) og vælger, om personen også må forbinde en AI og bygge egne apps («bygger»), eller kun åbne apps, der er delt med vedkommende.
- Hver app har en **ejer** og en synlighed under Indstillinger: *Kun mig* (standard), *Udvalgte personer*, *Alle med konto* eller *Offentlig*.
- Alle, der åbner en apps adresse, skal være logget ind, medmindre appen er offentlig. Dashboardet sender dem hjem til login og tilbage med en engangsbillet, og appens adresse sætter en cookie, der kun gælder for den ene app. Appen ser aldrig cookien.
- En AI handler som den bruger, den er forbundet med: den kan kun se og ændre den brugers egne apps. En ny app godkendes af sin ejer.
- Fjernes en persons bygge-ret, dør AI-adgangen med det samme. Fjernes personen, flyttes vedkommendes apps til ejeren og bliver private.
- Ejeren kan altid se og åbne alt. Apps kører på samme maskine, så inviter kun folk, du stoler på.
- Opgraderer du fra en tidligere version, bliver den første konto ejer, og eksisterende apps bliver **private**.

## Sådan virker koden

Se `ARCHITECTURE.md` for opbygningen: login-flows, bygge-pipelinen, lagring, git og routing.

## På en rigtig server

Som med ONCE er det én kommando på en ny Ubuntu/Debian-server:

```sh
curl -fsSL https://github.com/nicklasjeppesen/vibedeploy/releases/latest/download/install.sh | sh
```

Se `DEPLOY.md` for detaljer, installation uden terminal (Hetzners «Cloud config») og den manuelle vej. Den første konto kræver en opsætningskode: `vibedeploy install` viser den, og ellers står den i serverens log.

## Prøv det

Sig til Claude: *«Lav en lille hjemmeside til mit bageri og udgiv den på min server.
Brug deploy_app med en Dockerfile (nginx), og bed mig om en nøgle med request_secret, hvis du får brug for en.»*
Godkend i dashboardet, og åbn adressen.

## Hvad er med

| Del | Fil |
|---|---|
| Konto, login, sessioner (PBKDF2) | `auth.go`, `web.go` |
| OAuth 2.1 med PKCE og dynamisk registrering (så AI-værktøjer kan logge ind) | `oauth.go` |
| MCP over HTTP, 13 værktøjer | `mcp.go` |
| Byggevejledning og Dockerfile-skabeloner til AI'en, tjek af Dockerfile | `buildguide.go` |
| Lager i SQLite med krypterede hemmeligheder | `store.go`, `crypto.go` |
| Serverens CPU, hukommelse og disk (siden «Server») | `metrics*.go`, `serverpage.go` |
| Statusside på serverens IP under installationen | `setup.go` |
| Installation: `install.sh` og `vibedeploy install` | `install.sh`, `install.go` |
| Hentning fra git og deploy keys | `gitdeploy.go` |
| Færdige images fra Docker Hub/ghcr og registry-login | `imagedeploy.go` |
| Upload, data, backups, oprydning | `data.go` |
| Bygning, versioner, rollback, variabler, hemmeligheder | `deploy.go` |
| Docker og demo-runtime | `runtime.go` |
| Dashboard (skabeloner og CSS) | `web/` |

MCP-værktøjer: `get_build_guide`, `list_apps`, `deploy_app`, `prepare_upload`, `deploy_from_git`, `get_status`, `get_logs`, `set_env`, `request_secret`,
`rollback`, `stop_app`, `start_app`.

Sikkerhedsvalg: AI'en kan **aldrig læse** hemmeligheder (kun bede om en tom plads, som ejeren udfylder),
`set_env` afviser navne, der ligner hemmeligheder, og nye apps kræver ejerens klik, før de går live.
Fejler en ny version, kører den forrige videre.

## Kendt begrænset (prototype)

- **Docker-delen er kun delvist afprøvet:** `deploy_image` (hent, start, fejl ved privat image) er afprøvet mod rigtig Docker med images fra Docker Hub og ghcr.io. `docker build`-vejen er ikke. Demo-runtime og resten er testet.
- Mister du nøglen (`secret.key`), kan hemmelighederne ikke læses igen. Gem en kopi et sikkert sted, adskilt fra backuppen af data.
- Login-sessioner og AI-tokens gemmes ukrypteret i `state.db` (som før i state.json). Kun hemmeligheder krypteres.
- Historikken på siden «Server» ligger kun i hukommelsen og starter forfra, når Vibe Deploy genstartes.
- `install.sh` og `vibedeploy install` er tjekket for syntaks og med tests, og Caddyfilen er valideret med Caddy, men hele installationen er ikke kørt på en rigtig server endnu.
- Backup tages, mens appen kører. En app, der skriver i en database lige i det øjeblik, kan give en backup, der ikke er helt konsistent. Brug SQLite med WAL, eller stop appen før en manuel backup.
- `/data`-mappen har skriveadgang for alle (0777), fordi containere ofte kører som en anden bruger end serveren. Forældremappen er lukket (0700), så andre brugere på maskinen kan ikke komme ind.
- Én container og én port pr. app: ingen separat database eller flere tjenester (docker-compose).
- Ingen begrænsning af loginforsøg ud over en kort forsinkelse.
- Intet https lokalt. Brug Caddy foran på en rigtig server.
- Serveren kan starte containere, så den, der kan logge ind, har i praksis stor magt på maskinen.
  Kør den som en bruger uden unødige rettigheder, og eksponér ikke Docker-socketen.
- Eget domæne og skift af appnavn mangler.
- Invitationer sendes ikke på mail; ejeren kopierer linket og sender det selv.
- Ingen nulstilling af glemt adgangskode endnu.
- Hentning fra **private** repos over SSH er ikke afprøvet mod en rigtig GitHub-konto (testmiljøet kunne ikke nå port 22). Offentlige repos og nøgle-flowet er testet.
- Værtsnøglen for git-serveren godkendes første gang den ses (accept-new) og huskes derefter.
- Ingen refresh-tokens: adgangen udløber efter 90 dage, og AI-værktøjet skal godkendes igen.
