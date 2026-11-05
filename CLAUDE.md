# Cercle

Notes pour un agent de développement (Claude Code ou autre).

- Spécification d'origine : `docs/PRD.md`. Historique des jalons, écarts et décisions : `docs/JOURNAL.md`.
- Pas besoin de Go sur la machine : `deploy/go.sh` lance la chaîne Go dans Docker (`deploy/go.sh test -short ./...`, `deploy/go.sh vet ./...`, `deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...`).
- Avant un commit : `gofmt -l .` vide, `go vet`, `staticcheck`, tests ; `deploy/e2e.sh` pour tout changement réseau ou d'adhésion.
- Après un changement de dépendance : `deploy/licenses.sh`.
- Textes et commits en français, sans tiret cadratin : utiliser « : », « ; », « , » ou des parenthèses.
- Tout texte venu d'un pair (noms, chemins) est inséré comme texte dans l'interface, jamais comme HTML.
- Aucune transaction SQLite ne doit rester ouverte pendant un appel réseau (une seule connexion d'écriture : interblocage, voir `docs/JOURNAL.md`).
