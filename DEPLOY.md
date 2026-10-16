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
bygger `vibedeploy-linux-amd64`, `vibedeploy-linux-arm64` og lægger `install.sh` ved. Uden GitHub: sæt
`VIBEDEPLOY_URL` til et andet link (ARCH erstattes med amd64/arm64).

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
- **Docker-gruppen er i praksis root.** Den, der kan få tjenesten til at starte containere, har stor magt på maskinen. Hold serveren opdateret (`apt install unattended-upgrades`), og brug en stærk adgangskode.
- **Opdatering:** byg og upload en ny binær, derefter `systemctl restart vibedeploy`. Apps genstartes automatisk.
