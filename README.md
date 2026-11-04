# Cercle

Partage privé de fichiers entre les NAS d'un petit groupe d'amis, à travers un réseau WireGuard embarqué dans l'application. Spécification complète : [PRD.md](PRD.md).

Le projet est en cours de développement : les jalons J1 (tunnel) à J4 (adhésion) sont livrés. L'installation pour les membres sera documentée au jalon J6.

## Développement

Aucun Go n'est requis sur la machine : `deploy/go.sh` lance la chaîne Go dans Docker.

```sh
deploy/go.sh test -short ./...          # tests (sans le banc de débit)
deploy/go.sh vet ./...
deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@latest ./...
CGO_ENABLED=0 deploy/go.sh build -o /src/.bin/cercle ./cmd/cercle
```

### Essai à trois nœuds

La page web arrive en J5 ; en attendant, l'interface se pilote par son API JSON.

```sh
for i in 1 2 3; do
  NODE_NAME=n$i DATA_DIR=./n$i/data SHARE_DIR=./n$i/share DOWNLOADS_DIR=./n$i/dl \
  WG_LISTEN_PORT=5183$i WG_PUBLIC_ENDPOINTS=127.0.0.1:5183$i UI_LISTEN=127.0.0.1:1808$i UI_PASSWORD=secret \
  .bin/cercle serve &
done
curl -u x:secret -X POST http://127.0.0.1:18081/ui/api/circle            # n1 fonde le cercle (10.42.0.1)
curl -u x:secret -X POST http://127.0.0.1:18081/ui/api/invites           # renvoie {"code": "cercle1:…"}
curl -u x:secret -X POST http://127.0.0.1:18082/ui/api/join -d '{"code":"cercle1:…"}'
curl -u x:secret http://127.0.0.1:18081/ui/api/members                   # membres, en ligne, endpoints
curl -u x:secret -X POST http://127.0.0.1:18081/ui/api/members/<node_id>/revoke
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

## État

### J4 : adhésion (livré le 4 novembre 2026)

Ce qui marche :

- Création du cercle : le fondateur prend `10.42.0.1`.
- Invitation en une étape : un code `cercle1:…` (environ 450 caractères, les espaces et retours à la ligne ajoutés par une messagerie sont tolérés). Il sert une fois, expire après `INVITE_TTL`, et l'invitant peut l'annuler.
- Adhésion : le nouveau monte un tunnel provisoire avec la clé temporaire du code, sur une adresse temporaire, et appelle `/api/v1/join`. Il essaie chaque endpoint de l'invitant, puis bascule sur sa vraie clé. Si la réponse se perd, une nouvelle demande reçoit la même réponse.
- Fiches signées Ed25519 : admission signée par l'invitant (par lui-même pour le fondateur, l'identifiant du cercle dérivant de sa clé), corps signé par le membre. Une fiche falsifiée, ou sans chaîne d'invitation jusqu'au fondateur, est ignorée.
- Gossip : toutes les 30 s, et immédiatement après chaque changement (adhésion, révocation, nouvelle fiche), avec 2 pairs tirés au hasard, de préférence en ligne. Fusion par version, réconciliation WireGuard après chaque fusion.
- Endpoints : ceux annoncés par `WG_PUBLIC_ENDPOINTS` (noms résolus de nouveau toutes les 5 min), plus ceux sous lesquels les autres membres voient le pair. Rotation tant que le pair est injoignable ; une session vivante garde son endpoint.
- Révocation : par n'importe quel membre, signée, définitive (même face à une fiche de version plus haute), conservée au redémarrage. L'adresse du révoqué n'est jamais réattribuée.
- Conflit d'adresses : le membre entré le plus tôt garde l'adresse ; l'autre en tire une libre, republie sa fiche et redémarre son tunnel.
- Noms en double : un suffixe tiré de l'identifiant les rend uniques (« bob (a1b2) »), y compris pour les dossiers de téléchargement.
- Début de l'API de l'interface (prévue en J5) : `status`, `circle`, `join`, `invites`, `members`, `revoke`, avec `UI_PASSWORD` en authentification Basic. `GET /healthz` reste ouvert pour le healthcheck.
- La configuration statique de J1 a disparu.

Critères d'acceptation vérifiés par `internal/node` (trois nœuds complets, WireGuard réel sur 127.0.0.1) :

| Critère | Résultat |
| --- | --- |
| Un nœud neuf crée un cercle et obtient `10.42.0.1` | oui |
| Un second nœud rejoint avec un code, sans autre réglage | oui |
| Un troisième, invité par le deuxième, apparaît chez le premier en moins de 2 min, liaison directe | 0,5 s |
| Un code déjà utilisé ou expiré est refusé avec un message clair | « ce code d'invitation a déjà servi », « ce code d'invitation a expiré » |
| Un révoqué perd l'accès à tous les nœuds en moins de 2 min, y compris après redémarrage | 6 s (surtout les délais d'attente du test) |

Écarts au PRD (décisions de la relecture) :

- Pas de clé de groupe ni de HMAC : signatures Ed25519 par nœud (clé dans `/data/node.sign`, 0600). L'identifiant du cercle dérive de la clé du fondateur au lieu d'être aléatoire.
- Le code d'invitation contient une adresse temporaire en plus de l'adresse attribuée, et pas de clé de groupe.
- Endpoints observés publiés dans la fiche de l'observateur.
- Adresse attribuée tirée au hasard parmi les libres, au lieu de « la prochaine ».
- Table `members` : la fiche signée est stockée telle quelle (JSON), avec les colonnes utiles aux requêtes ; révocations dans une table à part.
- Un changement d'adresse redémarre le tunnel : les transferts en cours sont coupés puis repris par la file.

Ce qui manque :

- Un membre derrière NAT ne peut pas inviter s'il n'annonce aucun endpoint et qu'aucun autre membre ne le voit encore : l'invitation est refusée avec un message explicite.
- Pas encore d'essai entre deux vraies box : la traversée de NAT reste à mesurer sur le terrain.

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
- Le dossier du pair est figé à l'ajout : si le pair change de nom, ses fichiers suivants iront dans un nouveau dossier.
- Un fichier local de taille différente, au même emplacement, est remplacé à la fin du téléchargement.

Ce qui manque : l'API de la file pour l'interface (J5).

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
- (Remplacé en J4) cercle figé par un fichier `STATIC_CONFIG`.

Écarts au PRD :

- **Netstack maison** (`internal/wg/netstack.go`, ~200 lignes dérivées de `tun/netstack`, licence MIT). L'original lit un paquet par appel et garde les buffers TCP par défaut : 529 Mb/s. La version Cercle lit par lots de 128 et règle TCP (buffers auto-ajustés jusqu'à 8 Mio, CUBIC) : 1 310 Mb/s. Conséquence : `gvisor.dev/gvisor` est importé directement. Ce n'est pas un nouveau module, puisque `tun/netstack` en dépend déjà.
- **Endpoint et roaming** : la réconciliation n'écrase jamais l'endpoint d'un pair dont la session est vivante (poignée de main de moins de 3 min). Sinon, un pair derrière NAT, joignable seulement par son endpoint appris, serait coupé toutes les 30 s.
- `healthcheck` reporté à J6 : il interrogera `GET /healthz` de l'interface.

Ce qui manque ou reste à surveiller :

- `go test -race` signale une course **dans wireguard-go** (`conn.StdNetEndpoint.ClearSrc` contre `setSrcControl`, cache d'adresse source). Elle est sans gravité, mais elle fera échouer une CI qui lance `-race` sur `internal/wg`.
- Le premier contact entre deux nœuds démarrés en même temps prend environ 5 s : la première poignée de main se perd, puis WireGuard la réessaie.

### Décisions de la relecture du PRD

Prises le 4 novembre 2026 ; appliquées en J3 et J4, détaillées dans leurs sections. Tout membre peut inviter et révoquer. Plusieurs dossiers partagés : il suffit de monter plusieurs volumes sous `/share` (`/share/films`, `/share/musique`).

Question encore ouverte : combien de membres peuvent ouvrir un port UDP sur leur box. La réponse décidera s'il faut un relais après la v1.
