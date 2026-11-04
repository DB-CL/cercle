# Cercle

Partage privé de fichiers entre les NAS d'un petit groupe d'amis, à travers un réseau WireGuard embarqué dans l'application. Spécification complète : [PRD.md](PRD.md).

Le projet est en cours de développement : seul le jalon J1 (tunnel) est livré. L'installation pour les membres sera documentée au jalon J6.

## Développement

Aucun Go n'est requis sur la machine : `deploy/go.sh` lance la chaîne Go dans Docker.

```sh
deploy/go.sh test -short ./...          # tests (sans le banc de débit)
deploy/go.sh vet ./...
deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@latest ./...
CGO_ENABLED=0 deploy/go.sh build -o /src/.bin/cercle ./cmd/cercle
```

### Essai à deux nœuds (J1)

En J1, le cercle est figé par un fichier JSON (`STATIC_CONFIG`) ; l'adhésion par invitation arrive en J4.

```sh
DATA_DIR=./a .bin/cercle pubkey      # crée la clé de alice et affiche sa clé publique
DATA_DIR=./b .bin/cercle pubkey      # idem pour bob
```

`a/static.json` (le fichier de bob est symétrique) :

```json
{"circle_id": "demo", "tunnel_ip": "10.42.0.1",
 "peers": [{"name": "bob", "public_key": "<clé de bob>", "tunnel_ip": "10.42.0.2", "endpoint": "127.0.0.1:51832"}]}
```

```sh
NODE_NAME=alice DATA_DIR=./a STATIC_CONFIG=./a/static.json WG_LISTEN_PORT=51831 .bin/cercle serve
NODE_NAME=bob   DATA_DIR=./b STATIC_CONFIG=./b/static.json WG_LISTEN_PORT=51832 .bin/cercle serve
```

Chaque nœud interroge l'autre toutes les 30 s par `GET /api/v1/info` et journalise « pair en ligne ».

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

## État

### J1 : tunnel (livré le 4 novembre 2026)

Ce qui marche :

- Configuration complète par variables d'environnement, toutes les erreurs signalées d'un coup ; débits (`20M`, puissances de 1024) et créneaux (`22:00-06:00`, passage de minuit compris) déjà analysés pour J2 et J3.
- Clé du nœud créée au premier démarrage dans `/data/node.key` (0600, écriture atomique) ; une clé illisible est signalée, jamais remplacée.
- WireGuard embarqué, sans privilège ni `/dev/net/tun` ; réconciliation des pairs à chaud (ajout, mise à jour, retrait).
- API pair-à-pair sur `<ip_tunnel>:8787`, dans le netstack uniquement : rien n'écoute sur l'hôte. Middleware d'identité : IP source inconnue ou `X-Cercle-Circle` différent donnent `403`.
- Client pair-à-pair avec délais du PRD : 10 s pour se connecter, 60 s d'inactivité.
- Arrêt propre sur `SIGTERM`.
- Sous-commandes `serve`, `pubkey`, `version`.

Écarts au PRD :

- **Netstack maison** (`internal/wg/netstack.go`, ~200 lignes dérivées de `tun/netstack`, licence MIT). L'original lit un paquet par appel et garde les buffers TCP par défaut : 529 Mb/s. La version Cercle lit par lots de 128 et règle TCP (buffers auto-ajustés jusqu'à 8 Mio, CUBIC) : 1 310 Mb/s. Conséquence : `gvisor.dev/gvisor` est importé directement. Ce n'est pas un nouveau module, puisque `tun/netstack` en dépend déjà.
- **Endpoint et roaming** : la réconciliation n'écrase jamais l'endpoint d'un pair dont la session est vivante (poignée de main de moins de 3 min). Sinon, un pair derrière NAT, joignable seulement par son endpoint appris, serait coupé toutes les 30 s.
- `healthcheck` reporté à J5/J6 : il interroge l'interface, qui n'existe pas encore.

Ce qui manque ou reste à surveiller :

- `go test -race` signale une course **dans wireguard-go** (`conn.StdNetEndpoint.ClearSrc` contre `setSrcControl`, cache d'adresse source). Elle est sans gravité, mais elle fera échouer une CI qui lance `-race` sur `internal/wg`.
- Le premier contact entre deux nœuds démarrés en même temps prend environ 5 s : la première poignée de main se perd, puis WireGuard la réessaie.

### Points à trancher avant J4

Issus de la relecture du PRD du 4 novembre 2026 :

1. Endpoints observés : le PRD les met dans la fiche du pair observé, alors que seul ce pair peut incrémenter sa version. À la place, chaque nœud publie ce qu'il observe dans sa propre fiche.
2. Bascule de clé à l'invitation : remplacer la clé temporaire au même `allowed_ip` crée une course avec la réponse de `/join`. Solution : une adresse temporaire pour la clé temporaire, la vraie clé directement sur l'adresse attribuée.
3. Attribution d'adresse : « prochaine adresse libre » fait entrer en collision deux invitations simultanées presque à coup sûr. Un tirage aléatoire parmi les adresses libres, avec un départage stable (`joined_at` puis `node_id`), les rend rares. Un changement d'adresse oblige à recréer le netstack.
4. Révocation : `revoked` doit être absorbant à la fusion, indépendamment de la version.
5. HMAC à clé de groupe : il ne prouve que « écrit par un membre », ce que WireGuard garantit déjà. Une signature Ed25519 par nœud (bibliothèque standard) rend réellement applicable la règle « seul le nœud concerné modifie sa fiche ».
