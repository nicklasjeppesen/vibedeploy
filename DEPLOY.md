# Sæt Vibe Deploy op på en rigtig server

Mål: `https://dit-domæne.dk` er dashboardet og MCP-adressen, og hver app får `https://navn.dit-domæne.dk`.
Erstat `dit-domæne.dk` / `example.com` herunder med dit eget. Alle kommandoer på serveren køres som root.

**Status:** Caddyfilen er syntakstjekket, og serveren er afprøvet i «rigtig server»-tilstand lokalt
(opsætningskode, sikre cookies, certifikat-spørgsmål, app på underdomæne). Et fuldt forløb på en rigtig server med
Docker, Let's Encrypt og claude.ai er **ikke** afprøvet af mig. Forvent småjusteringer.

## Hurtigst: én kommando (som ONCE)

Log ind på en ny Ubuntu- eller Debian-server, og kør:

```sh
curl -fsSL https://github.com/nicklasjeppesen/vibedeploy/releases/latest/download/install.sh | sh
```

Scriptet (`install.sh`) installerer Docker, Caddy, git og firewall, henter `vibedeploy` fra GitHub Releases og kører
`vibedeploy install`. Den spørger om domæne og e-mail, laver nøglen til hemmeligheder og en opsætningskode, skriver
tjenestefilen og Caddyfilen (de samme som i `deploy/`) og starter det hele. Til sidst står opsætningskoden på skærmen.
Peg DNS på serveren, og åbn `http://SERVERENS-IP`: siden viser, når DNS og https er klar. Kommandoen kan køres igen sikkert.

**Helt uden terminal:** Indsæt `deploy/cloud-init.yaml` i Hetzners felt «Cloud config», og ret domæne, e-mail og en
opsætningskode, du selv vælger. Den kører samme kommando med svarene som miljøvariabler.

**Releases:** Scriptet henter fra GitHub Releases i `nicklasjeppesen/vibedeploy`. En ny release laves ved at pushe
et tag (`git tag v0.2.0 && git push --tags`). Workflowet i `.github/workflows/release.yml`
bygger `vibedeploy-linux-amd64`, `vibedeploy-linux-arm64` med versionsnummeret indbygget, signerer dem og lægger
`install.sh`, `release.json` og `release.json.sig` ved. Uden GitHub: sæt `VIBEDEPLOY_URL` til et andet link
(ARCH erstattes med amd64/arm64).

**Signering (én gang, før den første release):** Serverne installerer kun releases, der er signeret med projektets
nøgle. Kør på din Mac:

```sh
go run . keygen            # laver release-signing.key og viser den offentlige nøgle
```

1. Sæt den offentlige nøgle ind i `releasekey.go` (`var releasePublicKey = "…"`), og commit den.
2. Læg indholdet af `release-signing.key` i en GitHub-secret med navnet `VIBEDEPLOY_SIGNING_KEY`
   (repoets Settings → Secrets and variables → Actions). Gem en kopi et sikkert sted, fx i din password-manager.
   `.gitignore` udelukker `*.key`, men tjek alligevel med `git status`, at filen ikke er med, før du committer.
3. Push et tag som `v0.3.0` (formen `vX.Y.Z`; andre tags kan ikke sammenlignes og bliver ikke installeret).
   Workflowet nægter at signere, hvis nøglen ikke passer med `releasekey.go`.
4. **Den første version med nøglen skal installeres i hånden** på servere, der allerede kører: kør `install.sh` igen.
   En version uden nøgle i `releasekey.go` kan ikke tjekke releases og opdaterer sig derfor aldrig selv.

**To forskellige nøgler, som ikke må forveksles:**

| Fil | Hvor | Hvad den gør | Mister du den |
|---|---|---|---|
| `release-signing.key` | Din Mac + GitHub-secret | Signerer releases, så serverne stoler på dem | Serverne kan ikke opdatere sig selv længere (se nedenfor) |
| `secret.key` | Hver server (`/etc/vibedeploy/secret.key`) | Krypterer appenes hemmeligheder i `state.db` | Hemmelighederne på den server kan ikke læses igen |

`release-signing.key` skal aldrig ligge på en server, og `secret.key` skal aldrig i GitHub.

**Hvis `release-signing.key` bliver væk:** de installerede servere stoler kun på den gamle nøgle, så de kan ikke længere
opdatere sig selv. Lav et nyt nøglepar med `go run . keygen`, sæt den nye offentlige nøgle i `releasekey.go`, erstat
GitHub-secret'en, udgiv en ny version, og kør `install.sh` igen på hver server.

**Hvis `release-signing.key` er lækket** (fx kommet i git eller set af andre): gør det samme som ovenfor, med det samme.
Indtil hver server har fået den nye version via `install.sh`, så slå «Installér nye versioner automatisk» fra på siden
Server, og tryk ikke «Opdater nu». Serverne henter kun fra dit eget repos releases, så den, der har nøglen, skal også
kunne lægge filer i dine releases; men gå ud fra, at det kan ske. Er nøglen kommet i git, er den der stadig i
historikken, selvom du sletter filen.

**Stærkere værn:** den hemmelige nøgle ligger i GitHub, så signaturen beskytter mod ændrede filer i en release, men
ikke mod nogen, der har overtaget selve GitHub-kontoen. Vil du have det stærkeste værn, så lad være med at oprette
secret'en, og signér på din egen maskine: hent binærerne fra releasen til `dist/`, kør
`go run . sign -key release-signing.key -version v0.3.0 dist/`, og upload `release.json` og `release.json.sig` til releasen.

Resten af denne fil er den manuelle vej.

## 1. Server og domæne
1. Opret en Linux-server (Ubuntu 24.04) hos en EU-udbyder, fx Hetzner, Scaleway, OVH eller UpCloud. Brug en SSH-nøgle.
   Docker-builds kan bruge meget hukommelse; start gerne med mindst 2 GB RAM.
2. Hos din domæneudbyder: opret to DNS-poster, begge peger på serverens IP-adresse:
   - `A  @  → SERVER-IP`
   - `A  *  → SERVER-IP` (og tilsvarende `AAAA`, hvis serveren har IPv6)
3. Tjek (kan tage minutter til timer): `dig +short blog.dit-domæne.dk` skal vise server-IP'en.

**Uden eget domæne:** Brug en gratis adresse fra https://www.duckdns.org. Log ind, opret fx `annas-server`,
og skriv serverens IPv4-adresse ud for navnet. Alle underdomæner (`blog.annas-server.duckdns.org`) peger
automatisk på serveren, så du springer DNS-posterne over og bruger `annas-server.duckdns.org` som domæne
i resten af vejledningen. Brug ikke sslip.io eller nip.io: de står ikke på Public Suffix List, så alle
brugere deler Let's Encrypts certifikatgrænse, og https vil ofte fejle. `duckdns.org` står på listen.

## 2. Firewall
Kun SSH, http og https skal være åbne:
```sh
ufw allow OpenSSH && ufw allow 80 && ufw allow 443 && ufw enable
```
Vibe Deploy lytter kun på `127.0.0.1`, og app-containere publiceres kun på `127.0.0.1`, så de kan ikke nås direkte udefra.

## 3. Installér Docker og Caddy
```sh
curl -fsSL https://get.docker.com | sh
```
Til at hente apps fra git skal serveren også have git: `apt install -y git openssh-client`.
Caddy: følg «Debian/Ubuntu» på https://caddyserver.com/docs/install (installerer også en systemtjeneste).

## 4. Byg og upload programmet (fra din Mac)
Ligger din SSH-nøgle et andet sted end standardpladsen, så tilføj `-i` til `ssh` og `scp`, fx
`ssh -i ~/vibecoding_server/id_ed25519 root@SERVER-IP` og `scp -i ~/vibecoding_server/id_ed25519 vibedeploy root@SERVER-IP:/usr/local/bin/vibedeploy`.

Find serverens arkitektur: `uname -m` (x86_64 = amd64, aarch64 = arm64).
```sh
cd vibedeploy
GOOS=linux GOARCH=amd64 go build -o vibedeploy .     # eller GOARCH=arm64
scp vibedeploy root@SERVER-IP:/usr/local/bin/vibedeploy
scp deploy/vibedeploy.service root@SERVER-IP:/etc/systemd/system/
scp deploy/Caddyfile root@SERVER-IP:/etc/caddy/Caddyfile
```

## 5. Ret og start (på serveren)

Vil du også have opdatering fra dashboardet: kopiér `deploy/vibedeploy-update.service` og `deploy/vibedeploy-update.path`
til `/etc/systemd/system/`, og kør `systemctl enable --now vibedeploy-update.path`.

1. Åbn `/etc/systemd/system/vibedeploy.service` og erstat `example.com` (to steder) med dit domæne.
2. Åbn `/etc/caddy/Caddyfile` og erstat `example.com` (to steder) og e-mailadressen.
3. Opret en bruger og start:
```sh
chmod +x /usr/local/bin/vibedeploy
mkdir -p /etc/vibedeploy && (umask 077; head -c 32 /dev/urandom | base64 > /etc/vibedeploy/secret.key)
useradd --system --home /var/lib/vibedeploy --shell /usr/sbin/nologin vibedeploy
systemctl daemon-reload
systemctl enable --now vibedeploy
systemctl reload caddy
```

## 6. Opret din konto
Serveren kræver en **opsætningskode** til den første konto, så ingen anden kan nå det først:
```sh
journalctl -u vibedeploy | grep Opsætningskode
```
Åbn `https://dit-domæne.dk/signup` med det samme, og skriv koden ind sammen med dine oplysninger.
(Er koden væk, så genstart tjenesten, mens der endnu ingen konto er: den udskriver en ny.)

## 7. Forbind din AI
- Claude Code: `claude mcp add --transport http min-server https://dit-domæne.dk/mcp`, derefter `/mcp` → Authenticate.
- claude.ai / Claude-appen: tilføj en brugerdefineret forbindelse (Connectors) med `https://dit-domæne.dk/mcp`.

## Drift og sikkerhed
- **Backup:** tag backup af `/var/lib/vibedeploy`. Hemmelighederne i `state.db` er krypteret med nøglen i
  `/etc/vibedeploy/secret.key`, som **ikke** er i data-mappen. Gem en kopi af nøglen et andet sted end backuppen:
  uden den kan hemmelighederne ikke læses. Vibe Deploy lægger selv en kopi af databasen i `backups/_server/` hver nat.
- **Opgradering fra en version med state.json:** dataene flyttes automatisk til `state.db` ved første start. Har du den
  gamle tjenestefil uden `LoadCredential`, laves nøglen i `/var/lib/vibedeploy/secret.key`; flyt den til
  `/etc/vibedeploy/secret.key` og brug den nye tjenestefil. Slet `state.json.importeret`, når alt virker.
- **Certifikater:** Caddy henter ét certifikat pr. app, første gang adressen besøges. Let's Encrypt har grænser for, hvor mange der kan udstedes pr. tidsrum, så det er fint til få apps. Til mange apps: brug et wildcard-certifikat via Caddys DNS-udvidelse for din domæneudbyder.
- **Fejl med certifikater:** `journalctl -u caddy -e`.
- **Låst login:** efter 5 forkerte forsøg på en konto eller 20 fra én IP (inden for 15 minutter) låses login i 15
  minutter. Se hvem og hvorfra med `journalctl -u vibedeploy | grep "Login låst"`. Vil du ikke vente, så genstart
  tjenesten (`systemctl restart vibedeploy`); tællerne ligger kun i hukommelsen. Caddy sætter `X-Forwarded-For`,
  som Vibe Deploy kun stoler på, når forespørgslen kommer fra `127.0.0.1`.
- **Glemt adgangskode:** `sudo vibedeploy reset-password din@email.dk` giver et link til en ny adgangskode (virker én
  gang i en time). Kommandoen læser `/etc/vibedeploy/secret.key` (derfor `sudo`) og beder den kørende tjeneste på
  `127.0.0.1:8080` om at gemme linket; forespørgslen er signeret med en nøgle udledt af `secret.key`, så andre brugere
  på maskinen ikke kan lave links. Ligger nøglen et andet sted: `-key-file`. Tjenesten skal køre en version, der har
  kommandoen (v0.3.0 eller nyere), ellers svarer den med en fejl.
- **Docker-gruppen er i praksis root.** Den, der kan få tjenesten til at starte containere, har stor magt på maskinen. Hold serveren opdateret (`apt install unattended-upgrades`), og brug en stærk adgangskode.
- **Opdatering af Vibe Deploy:** under siden **Server** står, om der er en ny version. Tryk «Opdater nu», eller slå
  automatisk opdatering til og vælg hvornår: hver dag eller en bestemt ugedag, et klokkeslæt og en tidszone
  (en dropdown med 25 almindelige zoner på dansk, standard «Danmark (København)»; under «Andet …» kan enhver
  IANA-tidszone skrives, fx America/Toronto). Tidspunktet følger den
  valgte tidszone og dens sommertid, uanset serverens ur. Opdateringen går i gang inden for to timer efter tidspunktet, og
  hvert tidspunkt prøves kun én gang. Standard er hver dag kl. 03:00. Serveren tager en backup af `state.db` og lægger
  `update-request` i data-mappen; `vibedeploy-update.path` starter så `vibedeploy update` som root. Den henter den nyeste
  release, tjekker signatur og SHA-256, gemmer den gamle binær som `vibedeploy.prev`, skifter og genstarter tjenesten.
  Svarer den nye version ikke på `/healthz` inden for 60 sekunder, lægges den gamle tilbage (og kun hvis den ikke kan
  starte på de nuværende data, også backuppen af `state.db`). Appenes containere kører videre imens, og Caddy lader
  forespørgsler vente (`lb_try_duration`), mens tjenesten genstarter. Forløbet: `journalctl -u vibedeploy-update`.
  Fra terminalen: `vibedeploy update -check` (se, om der er en ny) og `systemctl start vibedeploy-update` (opdatér nu).
- **Opgradering af en server fra før selvopdatering:** kør `install.sh` (eller `vibedeploy install`) igen én gang.
  Den henter den nye binær og opretter de to enheder til opdatering.
- **Manuel opdatering:** byg og upload en ny binær, derefter `systemctl restart vibedeploy`. Kørende apps overtages
  uden genstart.
