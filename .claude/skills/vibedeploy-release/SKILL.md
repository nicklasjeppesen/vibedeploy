---
name: vibedeploy-release
description: Udgiv en ny, signeret version af Vibe Deploy (tag, GitHub Actions-release, tjek af signatur). Brug den, når ejeren beder om en ny release eller version af Vibe Deploy.
---

# Udgiv en ny version af Vibe Deploy

Servere med Vibe Deploy installerer kun releases, der er signeret med projektets nøgle og har en nyere version
(`vX.Y.Z`). En release, der ikke er signeret rigtigt, bliver ikke installeret: serverne melder en fejl og bliver på
den version, de har. Følg trinene i rækkefølge, og stop og spørg ejeren, hvor der står det.

## 1. Tjek, at alt er klar

```sh
git status                 # skal være ren, og du skal være på main
gofmt -l .                 # skal være tom
go vet ./... && go test -race ./...
```

- `releasekey.go` skal have en offentlig nøgle (`var releasePublicKey = "…"`, ikke tom). Er den tom: stop, og bed
  ejeren køre `go run . keygen` og følge «Signering» i `DEPLOY.md`. **Lav aldrig selv nøglen**: den hemmelige del
  må kun findes hos ejeren og i GitHub-secret'en `VIBEDEPLOY_SIGNING_KEY`.
- Tjek, at ingen nøglefil er på vej i git: `git ls-files | grep -E '\.key$'` skal være tom.

## 2. Vælg versionen

```sh
git tag --sort=-v:refname | head -5
```

- Formen er altid `vX.Y.Z` (ingen `-rc`, ingen foranstillede nuller). Andre tags bliver ikke installeret.
- Patch (`Z`) til rettelser, minor (`Y`) til nye funktioner. Er `schemaVersion` i `store.go` hævet siden sidste
  release, så nævn det i release-teksten: en ældre version kan ikke køre på data fra den nye.
- Foreslå versionen og en kort liste over ændringerne (`git log <sidste-tag>..HEAD --oneline`), og **få ejerens
  ja**, før du går videre. Et pushet tag starter en release, som servere med automatik installerer på deres valgte tidspunkt.

## 3. Tag og push

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
```

Workflowet `.github/workflows/release.yml` tester, bygger `vibedeploy-linux-amd64` og `-arm64` med versionen
indbygget, signerer (`go run . sign`) og udgiver med `install.sh`, `release.json` og `release.json.sig`.

## 4. Følg med og tjek resultatet

```sh
gh run list --workflow Release --limit 3
gh run watch                      # vælg kørslen for tagget
gh release view vX.Y.Z            # skal have 5 filer
```

- Står der en advarsel om, at `VIBEDEPLOY_SIGNING_KEY` mangler, er releasen usigneret: sig det til ejeren.
  Serverne opdaterer sig ikke til den, før secret'en er sat og `release.json` + `.sig` er lagt på releasen
  (se «Stærkere værn» i `DEPLOY.md` for at signere på Mac'en og uploade filerne).
- Fejler «Signér» med «nøglen passer ikke med releasePublicKey», er secret'en og `releasekey.go` fra hvert sit
  nøglepar. Ret det, og lav en ny patch-version; genbrug ikke et tag.

Tjek signaturen, som en server ville gøre det (versionen her er bare et lavt tal, så den nye ses som nyere):

```sh
VIBEDEPLOY_UPDATE_URL=https://github.com/nicklasjeppesen/vibedeploy/releases/download/vX.Y.Z \
  go run -ldflags "-X main.version=v0.0.0" . update -check
```

Den skal svare «Ny version: vX.Y.Z». En fejl om signaturen betyder, at serverne ikke vil installere den.

## 5. Fortæl ejeren

- Hvilken version der er udgivet, og at servere med automatik installerer den på det tidspunkt, ejeren har valgt
  (standard hver dag kl. 03:00 i Europe/Copenhagen), mens andre kan trykke «Opdater nu» på siden Server.
- Er det **den første version med en nøgle i `releasekey.go`**, skal hver server, der allerede kører, have den i
  hånden én gang: `install.sh` igen (se `DEPLOY.md`). Ældre versioner kan ikke tjekke signaturer.
- Svarer en ny version ikke på en server, ruller den selv tilbage; status står på siden Server og i
  `journalctl -u vibedeploy-update`.
