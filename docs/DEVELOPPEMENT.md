# Développement

Aucun Go n'est requis sur la machine : `deploy/go.sh` lance la chaîne Go dans Docker.

```sh
docker build -f deploy/Dockerfile -t cercle .   # image (environ 15 Mo) ; ajoutez --network host si les étapes RUN manquent de DNS
deploy/e2e.sh                                                   # 5 conteneurs, scénario complet
```

```sh
deploy/go.sh test -short ./...          # tests (sans le banc de débit)
deploy/go.sh vet ./...
deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@latest ./...
CGO_ENABLED=0 deploy/go.sh build -o /src/.bin/cercle ./cmd/cercle
```

## Essai à trois nœuds

L'interface est sur `http://127.0.0.1:18081/` (puis 18082, 18083), mot de passe `secret` ; `curl` peut s'authentifier en Basic (`-u x:secret`). Elle se pilote aussi par son API JSON ; toute requête qui modifie quelque chose doit porter l'en-tête `X-Cercle-UI` (protection CSRF).

```sh
for i in 1 2 3; do
  NODE_NAME=n$i DATA_DIR=./n$i/data SHARE_DIR=./n$i/share DOWNLOADS_DIR=./n$i/dl \
  WG_LISTEN_PORT=5183$i WG_PUBLIC_ENDPOINTS=127.0.0.1:5183$i UI_LISTEN=127.0.0.1:1808$i UI_PASSWORD=secret \
  .bin/cercle serve &
done
C='curl -u x:secret -H X-Cercle-UI:1'
$C -X POST http://127.0.0.1:18081/ui/api/circle            # n1 fonde le cercle (10.42.0.1)
$C -X POST http://127.0.0.1:18081/ui/api/invites           # renvoie {"code": "cercle1:…"}
$C -X POST http://127.0.0.1:18082/ui/api/join -d '{"code":"cercle1:…"}'
$C http://127.0.0.1:18081/ui/api/members                   # membres, en ligne, endpoints
$C -X POST http://127.0.0.1:18082/ui/api/queue -d '{"peer":"<node_id de n1>","paths":["films"]}'
```

`DATA_DIR`, `SHARE_DIR` et `DOWNLOADS_DIR` (défauts `/data`, `/share`, `/downloads`) ne servent qu'au développement hors conteneur.

## Banc de débit

```sh
CERCLE_BENCH_MB=2048 deploy/go.sh test ./internal/wg -run TestThroughput -v -count=1
```

Deux nœuds complets dans le même processus, reliés en UDP sur `127.0.0.1`. Les deux bouts du tunnel partagent le même processeur : c'est une borne basse du débit entre deux machines de cette puissance.

Mesures du 4 novembre 2026, AMD Ryzen 7 2700X, transfert TCP de 512 Mio :

| Cœurs utilisés | Netstack Cercle | Netstack wireguard-go d'origine |
| --- | --- | --- |
| 16 | 1 310 Mb/s | 529 Mb/s |
| 4 | 1 351 Mb/s | 814 Mb/s |
| 2 | 945 Mb/s | 462 Mb/s |

Pas encore de mesure sur arm64 : il faut un vrai NAS, l'émulation QEMU ne dit rien des performances.


## Organisation du code

| Paquet | Rôle |
| --- | --- |
| `cmd/cercle` | Point d'entrée : `serve`, `healthcheck`, `pubkey`, `version` |
| `internal/config` | Variables d'environnement, débits, créneaux, réglages en base |
| `internal/wg` | WireGuard embarqué, netstack (dérivé de `tun/netstack`), réconciliation des pairs |
| `internal/membership` | Cercle, fiches signées Ed25519, invitations, révocations, fusion des tables |
| `internal/index` | Scan et index du partage, sélection des dossiers, confinement des chemins |
| `internal/peerapi` | API HTTP entre nœuds (dans le tunnel) et son client |
| `internal/transfer` | Brides d'envoi, file de téléchargement, reprise |
| `internal/store` | SQLite et migrations |
| `internal/node` | Assemblage d'un nœud, gossip, cycle de vie du tunnel |
| `internal/ui` | API locale, connexion, événements en direct, page embarquée |
| `web` | Page unique (HTML, CSS, JavaScript sans framework) |
| `deploy` | Dockerfile, compose, test de bout en bout, outils |

La spécification d'origine est dans [PRD.md](PRD.md), l'historique des jalons et des choix dans [JOURNAL.md](JOURNAL.md).

## Licences des dépendances

`deploy/licenses.sh` régénère `THIRD_PARTY_LICENSES.md` après tout changement de dépendance ; ce fichier est copié dans l'image.
