# Cercle

`PRD.md` est la source de vérité : le lire avant toute modification. L'état d'avancement et les écarts au PRD sont dans la section « État » du `README.md`.

- Pas de Go sur l'hôte : toute la chaîne passe par `deploy/go.sh` (Docker, `golang:1.27.3`, `--network host`). Exemples : `deploy/go.sh test -short ./...`, `deploy/go.sh vet ./...`, `deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@latest ./...`.
- Format : `docker run --rm -u 1000:1000 -v $PWD:/src -w /src golang:1.27.3-bookworm gofmt -l .` doit être vide.
- Travail jalon par jalon ; plan bref avant chaque jalon, validation de Cyril s'il s'écarte du PRD.
- Commits en français, sans mention de Claude ni ligne Co-Authored-By.
- Français : pas de tiret cadratin, utiliser : ; , ou des parenthèses.
- Le port UDP 51820 est déjà pris sur la machine de dev (WireGuard de Cyril) : tester sur 51830 et plus.
- Bout en bout : `deploy/e2e.sh` (5 conteneurs, environ 2 min). Image : `docker build --network host -f deploy/Dockerfile -t cercle .`
