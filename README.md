# Cercle

Partage privé de fichiers entre les NAS d'un petit groupe d'amis, à travers un réseau WireGuard embarqué dans l'application. Spécification complète : [PRD.md](PRD.md).

Le projet est en cours de développement : les jalons J1 (tunnel), J2 (partage) et J3 (téléchargement) sont livrés. L'installation pour les membres sera documentée au jalon J6.

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

### J3 : téléchargement (livré le 4 novembre 2026)

Ce qui marche :

- File persistante en base. Un dossier mis en file est développé en fichiers au moment de l'ajout. Un fichier déjà présent avec la même taille est marqué terminé sans transfert, et une remise en file ne crée pas de doublon.
- `DOWNLOAD_CONCURRENCY` transferts au total, un seul par pair. Ordre : priorité, puis date d'ajout.
- Reprise : écriture dans `<destination>.part`, reprise par `Range` et `If-Range`. Si le fichier a changé chez le pair, le transfert repart de zéro avec la nouvelle version. À la fin : vérification de la taille, `fsync`, renommage atomique, date de modification distante à la nanoseconde (en-tête `X-Cercle-Mtime`).
- Erreurs : coupure ou pair injoignable donnent un nouvel essai après 30 s, puis ×2, plafonné à 15 min, sans limite de tentatives. Un `503` attend le `Retry-After` du pair sans compter d'échec. Un `404`, un chemin refusé ou un pair inconnu mettent l'élément en erreur définitive.
- `DOWNLOAD_LIMIT` (débit total) et `DOWNLOAD_WINDOW`, réglables à chaud.
- Actions : pause, reprise, priorité haute, retrait (avec ou sans suppression du `.part`), relance d'une erreur.
- Redémarrage : la file est conservée et les transferts interrompus reprennent au bon octet.
- Confinement côté réception : chaque chemin reçu d'un pair est revalidé, le nom du pair est rendu sûr pour servir de dossier (`../../etc` devient `_.._etc`), et toutes les écritures passent par `os.Root` sur `/downloads`.

Mesures (tunnel WireGuard réel, deux nœuds dans le même processus) :

| Test | Résultat |
| --- | --- |
| 501 fichiers (dont un de 20 Mio) mis en file puis téléchargés | 7,7 s, sommes de contrôle identiques |
| 10 001 fichiers (petits, plus un de 20 Mio) | 1 min 53, sommes de contrôle identiques (environ 11 ms par fichier : une requête, un `fsync` et un renommage chacun, un seul transfert à la fois par pair) |

Écarts au PRD :

- Colonnes `peer_name` et `mtime` ajoutées à `queue`.
- La fermeture du créneau de téléchargement n'interrompt pas un transfert en cours : elle empêche seulement d'en démarrer de nouveaux (même comportement que le créneau d'upload).
- Le dossier du pair est figé à l'ajout : si le pair change de nom, ses fichiers suivants iront dans un nouveau dossier. Deux pairs de même nom partagent un dossier ; les noms seront rendus uniques en J4.
- Un fichier local de taille différente, au même emplacement, est remplacé à la fin du téléchargement.

Ce qui manque : aucun moyen de mettre en file sans l'interface (J5). En attendant, la file se teste par `go test`.

### J2 : partage (livré le 4 novembre 2026)

Ce qui marche :

- Base SQLite (`/data/cercle.db`) en WAL, une seule connexion d'écriture, migrations numérotées embarquées dans le binaire.
- Scan du partage : au démarrage, puis toutes les `SCAN_INTERVAL`. Une seule goroutine, aucune lecture de contenu. Exclusions du PRD et `SHARE_EXCLUDE` (motif testé sur le nom et sur le chemin). Taille et nombre de fichiers cumulés par dossier.
- `GET /api/v1/files` (pages de 500, dossiers d'abord puis nom sans tenir compte de la casse), `GET /api/v1/tree` (NDJSON), `GET /api/v1/file` (`Range`, `If-Range`, `ETag` = `"<taille>-<mtime_ns>"`).
- Confinement des chemins : `..`, chemin absolu et octet nul refusés (`400`). Seuls les fichiers présents dans l'index sont servis (un fichier caché ou exclu donne `404`, même si l'on devine son nom). `os.Root` interdit toute sortie du partage, liens symboliques compris.
- Brides d'upload réglables à chaud : débit total partagé (`UPLOAD_LIMIT`), `MAX_UPLOADS` (`503` + `Retry-After: 60` au-delà), `UPLOAD_WINDOW` (`503` + `Retry-After` jusqu'à l'ouverture).
- Réglages en base prioritaires sur l'environnement (`ApplySettings`), prêts pour l'interface de J5.
- `GET /api/v1/info` renvoie la taille, le nombre de fichiers et la date du dernier scan.

Mesures :

| Test | Résultat | Cible du PRD |
| --- | --- | --- |
| Scan de 500 000 fichiers | 13 s, 18 Mio de mémoire au maximum | moins de 200 Mio |
| Rescan des mêmes 500 000 fichiers | 16 s | |
| Liste d'un dossier dans cet index | 0,5 ms | moins de 200 ms |
| `UPLOAD_LIMIT=10M`, 3 téléchargeurs | 10,07 Mio/s au total | 9 à 11 |

Écarts au PRD :

- Colonne `name` ajoutée à `files`, pour trier sans recalculer le nom.
- Les suffixes de débit sont en puissances de 1024 : `10M` = 10 Mio/s.
- Liens symboliques : vers un fichier du partage, indexé ; vers un dossier, ignoré (risque de boucle) ; sortant du partage, ignoré.

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

### Décisions pour les jalons suivants

Prises le 4 novembre 2026 après relecture du PRD ; elles l'emportent sur lui là où elles le contredisent.

- **J3** : les chemins reçus d'un pair et son nom sont confinés sous `/downloads` (`os.Root`). Un `503` respecte le `Retry-After` sans passer par le backoff ni compter comme un échec.
- **J4, endpoints observés** : chaque nœud publie ce qu'il observe dans sa propre fiche, et non dans celle du pair observé.
- **J4, invitation** : la clé temporaire reçoit une adresse temporaire, et la vraie clé est enregistrée directement sur l'adresse attribuée. Il n'y a donc aucune bascule de clé sur une même adresse.
- **J4, adresses** : tirage au hasard parmi les adresses libres, départage par `joined_at` puis `node_id`.
- **J4, révocation** : `revoked` l'emporte à la fusion, quelle que soit la version.
- **J4, intégrité des fiches** : signature Ed25519 par nœud (bibliothèque standard) à la place du HMAC à clé de groupe. Une révocation est signée par le membre qui la prononce. La rotation de la clé de groupe devient inutile.
- **Droits** : tout membre peut inviter et révoquer, comme dans le PRD.
- **Plusieurs dossiers partagés** : déjà possible, en montant plusieurs volumes sous `/share` (`/share/films`, `/share/musique`).

Question encore ouverte : combien de membres peuvent ouvrir un port UDP sur leur box. La réponse décidera s'il faut un relais après la v1.
