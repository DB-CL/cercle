# PRD Cercle : partage privé de fichiers entre NAS sur WireGuard

4 novembre 2026 · Cyril

## Contexte et objectif

Cercle est une image Docker unique qui permet à un petit groupe d'amis (2 à 20 personnes) d'échanger de gros volumes de fichiers entre leurs NAS, allumés en permanence, via un réseau WireGuard embarqué dans l'application.

Chaque membre lance le même conteneur. Depuis une page web sur son LAN, il voit la liste des membres, parcourt les dossiers partagés de chacun, coche ce qu'il veut, et les fichiers se téléchargent en arrière-plan sur son NAS. Chacun limite le débit qu'il accepte de servir aux autres.

Ce que le produit remplace : des liens de transfert ponctuels, un tracker torrent privé ou un hub Direct Connect, jugés trop lourds pour ce besoin.

Trois principes guident chaque décision :

- **Tout passe par WireGuard.** Aucun service n'est exposé hors du tunnel, à part le port UDP de WireGuard lui-même.
- **Zéro configuration réseau manuelle.** Les pairs, leurs clés et leurs adresses se découvrent dans l'application, à partir d'un code d'invitation.
- **Minimal.** Un binaire Go statique, peu de dépendances, une page HTML, une base SQLite.

## Utilisateurs, cas d'usage et non-objectifs

Les utilisateurs sont des particuliers à l'aise avec Docker, qui possèdent un NAS (Synology, QNAP, Unraid, TrueNAS, serveur Linux) en amd64 ou arm64. Ils se connaissent et se font confiance : tous les membres ont les mêmes droits.

Cas d'usage couverts :

1. **Créer un cercle.** Le premier membre démarre le conteneur et clique sur « Créer un cercle ». Il obtient l'adresse 10.42.0.1.
2. **Inviter un ami.** Un membre génère un code d'invitation et l'envoie par messagerie. L'ami lance le conteneur, colle le code, et rejoint le cercle sans autre réglage.
3. **Parcourir.** Un membre voit qui est en ligne, ouvre le partage d'un autre et navigue dans ses dossiers.
4. **Télécharger.** Il coche des fichiers ou des dossiers entiers. Ils rejoignent une file qui se vide en arrière-plan, avec reprise après coupure ou indisponibilité du pair.
5. **Limiter.** Chacun fixe son débit d'upload maximal, son nombre d'uploads simultanés et, en option, des créneaux horaires.
6. **Retirer un membre.** Un membre en révoque un autre ; tous les nœuds le retirent de WireGuard.

Non-objectifs (hors périmètre, ne pas implémenter) :

- Lecture en streaming, aperçu ou miniatures : on télécharge, on ne visionne pas.
- Application desktop ou mobile.
- Partage public par lien, ou accès hors du cercle.
- Téléchargement multi-sources (essaim BitTorrent) : un fichier vient d'un seul pair.
- Chat, commentaires, notifications.
- Traversée de NAT garantie : voir la section joignabilité.
- Rôles et permissions différenciés en v1.

## Principes d'architecture

Chaque nœud est identique et autonome : il n'existe aucun serveur central, seulement des pairs qui partagent la même table des membres.

- **Un binaire, un conteneur, un port.** Seul le port UDP de WireGuard est exposé hors du LAN.
- **Deux plans séparés.** L'interface écoute sur le LAN (`:8080`). L'API pair-à-pair écoute uniquement sur l'adresse de tunnel (`:8787`), via le netstack.
- **HTTP ordinaire dans WireGuard.** Le chiffrement et l'authentification viennent du tunnel ; l'identité d'un appelant est son IP source dans le tunnel.
- **État local.** Clé, cercle, membres, index et file vivent dans SQLite sous `/data`.

(Schéma d'architecture d'un nœud, 6 composants et 1 port exposé : voir le document Claude Docs d'origine.)

Les requêtes sortantes (téléchargements, gossip) et entrantes (navigation, fichiers) traversent toutes le même WireGuard embarqué, que le module d'adhésion reconfigure à chaud.

## Exigences fonctionnelles : réseau et adhésion

### Identité

- Au premier démarrage, le nœud génère une paire de clés WireGuard (Curve25519) et l'enregistre dans `/data/node.key` (droits 0600). L'identifiant du nœud (`node_id`) est sa clé publique en base64.
- Le nom affiché vient de `NODE_NAME` et peut être modifié dans l'interface.
- Tant que le nœud n'appartient à aucun cercle, l'interface propose deux actions : « Créer un cercle » ou « Rejoindre avec un code ».

### WireGuard embarqué

- WireGuard tourne entièrement dans le processus : `wireguard-go` avec le TUN `netstack` (pile TCP/IP userspace gVisor). Pas de module noyau, pas de `NET_ADMIN`, pas de `/dev/net/tun`, pas de `network_mode: host`.
- Le nœud écoute en UDP sur `WG_LISTEN_PORT` (défaut 51820). C'est le seul port à exposer hors du LAN.
- Le nœud a une adresse de tunnel IPv4 dans le sous-réseau du cercle (défaut `10.42.0.0/24`). L'API pair-à-pair écoute en HTTP sur `<ip_tunnel>:8787`, via le netstack uniquement.
- La configuration des pairs est appliquée à chaud par `IpcSet` (clé publique, `allowed_ip` en /32, `endpoint`, `persistent_keepalive_interval=25`). Ajout, mise à jour et retrait de pairs se font sans redémarrage.
- L'IP source dans le tunnel est authentifiée par WireGuard. Toute requête entrante est attribuée au membre dont c'est l'adresse de tunnel ; une adresse inconnue est rejetée.

### Création du cercle

- Le fondateur reçoit `10.42.0.1`. Le nœud génère un `circle_id` (16 octets aléatoires) et une clé de groupe (32 octets aléatoires), stockés dans `/data`.
- Le sous-réseau est configurable à la création (`TUNNEL_SUBNET`) et figé ensuite.

### Invitation (une seule étape pour l'utilisateur)

Le problème à résoudre : l'invitant doit connaître la clé publique du nouveau avant que la poignée de main WireGuard puisse réussir. On le contourne avec une clé temporaire.

1. L'invitant clique « Inviter ». Son nœud attribue la prochaine adresse libre, génère une paire de clés temporaire et un jeton à usage unique, et ajoute la clé publique temporaire comme pair avec l'adresse attribuée.
2. Le code d'invitation contient : `circle_id`, clé de groupe, sous-réseau, adresse attribuée, clé privée temporaire, jeton, clé publique, adresse de tunnel et endpoints de l'invitant. Il est sérialisé en JSON compact, puis encodé en base64url avec un préfixe `cercle1:`.
3. Le nouveau colle le code. Son nœud monte le tunnel avec la clé temporaire et appelle `POST /api/v1/join` en présentant sa vraie clé publique, son nom et le jeton.
4. L'invitant vérifie le jeton, remplace la clé temporaire par la vraie clé, crée la fiche du membre et la renvoie avec la table des membres complète. Le nouveau bascule alors sur sa vraie clé.
5. La fiche se propage aux autres membres par gossip.

Contraintes : un code expire après 72 heures (configurable) et ne sert qu'une fois. La clé temporaire n'a accès qu'à `/api/v1/join`. L'invitant voit ses invitations en attente et peut les annuler.

### Table des membres et gossip

- Chaque nœud conserve une table de fiches membres : `node_id`, nom, adresse de tunnel, endpoints annoncés, endpoints observés, `version`, `updated_at`, `revoked`, `invited_by`.
- Chaque fiche porte un HMAC-SHA256 calculé avec la clé de groupe. Une fiche au HMAC invalide est ignorée.
- Toutes les 30 secondes, chaque nœud choisit 2 pairs en ligne au hasard et échange sa table complète (`POST /api/v1/gossip`). Les tables sont petites : pas de digest nécessaire en v1.
- Fusion : pour un même `node_id`, la fiche de plus grande `version` gagne. Seul le nœud concerné incrémente sa propre version, sauf pour la révocation.
- Après chaque fusion, le nœud réconcilie sa configuration WireGuard avec la table.

### Révocation

- N'importe quel membre peut révoquer un autre membre depuis l'interface. La fiche passe `revoked=true` avec une version très haute, et cet état est définitif.
- Tous les nœuds retirent le pair de WireGuard dès réception. L'adresse de tunnel n'est jamais réattribuée.
- La clé de groupe n'est pas tournée en v1. La rotation est une question ouverte.

### Conflits d'adresses

Deux invitations simultanées peuvent attribuer la même adresse. Règle : la fiche au `joined_at` le plus ancien garde l'adresse. L'autre nœud détecte le conflit, prend la plus petite adresse libre, incrémente sa version et republie sa fiche.

### Joignabilité et endpoints

- Un nœud annonce les endpoints de `WG_PUBLIC_ENDPOINTS` (liste `hôte:port`, IPv4, IPv6 ou nom DynDNS). Les noms sont résolus à nouveau toutes les 5 minutes.
- Chaque nœud lit par `IpcGet` l'endpoint réel de ses pairs (roaming WireGuard) et le publie dans la fiche du pair, comme endpoint observé. Un nœud non joignable devient ainsi trouvable par les autres.
- Pour joindre un pair, le nœud essaie dans l'ordre : endpoints annoncés, puis endpoints observés, avec keepalive à 25 s. Quand deux nœuds derrière NAT connaissent chacun l'endpoint observé de l'autre, leurs keepalives simultanés peuvent percer des NAT simples. C'est un bonus, pas une garantie.
- Hypothèse v1 : au moins un membre est joignable, et chaque paire de membres compte au moins un côté joignable.
- Hors v1 : relais applicatif. Un membre joignable sert de proxy HTTP (`/api/v1/relay/{node_id}/...`) entre deux membres qui ne se voient pas. Le routage IP via netstack est écarté : il demanderait d'activer le forwarding dans gVisor, ce qui est trop complexe pour le gain.
- L'interface affiche pour chaque pair : en ligne ou non, dernière poignée de main, endpoint utilisé, débit courant.

## Exigences fonctionnelles : fichiers et interface

### Index du partage

- Le dossier partagé est monté en lecture seule sur `/share`. Un scan périodique (`SCAN_INTERVAL`, défaut 1 h) l'indexe dans SQLite : chemin relatif, taille, date de modification, dossier ou fichier. Le scan est aussi déclenchable depuis l'interface.
- Le scan est incrémental : il compare taille et mtime, sans lire le contenu. Il ne doit pas saturer les disques du NAS (une seule goroutine, pas de hash par défaut).
- Exclusions : fichiers et dossiers cachés (`.` en tête), `@eaDir`, `#recycle`, `.Trash-*`, `$RECYCLE.BIN`, plus les motifs de `SHARE_EXCLUDE`.
- Les liens symboliques sortant de `/share` sont ignorés.
- Chaque dossier porte sa taille cumulée et son nombre de fichiers, calculés au scan.

### Navigation

- Le pair expose le contenu d'un dossier à la fois, paginé (`GET /api/v1/files?path=&cursor=`), trié dossiers d'abord puis par nom.
- Un dossier peut être développé récursivement à la mise en file (`GET /api/v1/tree?path=`), qui renvoie tous les fichiers sous ce chemin.
- Recherche par nom dans le partage d'un pair : hors v1, la question reste ouverte.

### Service des fichiers et limite d'upload

- `GET /api/v1/file?path=` sert le fichier avec `http.ServeContent` : support de `Range`, `If-Range`, et `ETag` = `"<taille>-<mtime_unix_nano>"`.
- Le flux sortant passe par un limiteur global à jeton (`golang.org/x/time/rate`), partagé par tous les uploads, réglé par `UPLOAD_LIMIT` (octets/s, suffixes K, M, G ; `0` = illimité).
- `MAX_UPLOADS` (défaut 3) borne les uploads simultanés. Au-delà, la réponse est `503` avec `Retry-After: 60`.
- `UPLOAD_WINDOW` optionnel (ex. `01:00-07:00`, heure locale `TZ`) : hors créneau, réponse `503` avec `Retry-After` jusqu'au début du créneau.
- Les deux limites sont modifiables à chaud depuis l'interface. La valeur stockée en base prime sur la variable d'environnement après la première modification.

### File de téléchargement

- Mettre en file un dossier le développe en fichiers individuels au moment de l'ajout. Un élément = un fichier distant.
- Destination : `/downloads/<nom du pair>/<chemin distant>`. Si le fichier existe déjà avec la même taille, l'élément est marqué terminé sans transfert.
- Workers : `DOWNLOAD_CONCURRENCY` (défaut 2) au total, au plus 1 transfert par pair. Ordre : priorité puis date d'ajout.
- Reprise : écriture dans `<destination>.part`, reprise par `Range: bytes=<taille du .part>-` avec `If-Range` sur l'ETag. Si l'ETag a changé, le transfert repart de zéro. À la fin : vérification de la taille, `fsync`, renommage atomique, puis application du mtime distant.
- Erreurs : pair hors ligne, `503` ou coupure entraînent un nouvel essai avec backoff exponentiel (30 s, puis ×2, plafond 15 min), sans limite de tentatives. Un `404` ou un chemin refusé met l'élément en erreur définitive.
- `DOWNLOAD_LIMIT` et `DOWNLOAD_WINDOW` optionnels, sur le même modèle que l'upload.
- Actions sur un élément ou une sélection : pause, reprise, priorité haute, retrait (avec ou sans suppression du `.part`), relance d'une erreur.
- La file survit aux redémarrages du conteneur.

### Interface web

- Une seule page, en français, HTML, CSS et JavaScript vanilla embarqués dans le binaire (`embed`). Pas de framework, pas de build front, pas de CDN.
- Servie sur `UI_LISTEN` (défaut `:8080`), côté LAN, jamais sur l'adresse de tunnel. Protection optionnelle par `UI_PASSWORD` (authentification Basic). Avertissement affiché si elle est absente.
- Vues :
  1. **Membres** : liste avec statut en ligne, taille partagée, dernière vue ; actions inviter et révoquer.
  2. **Partage d'un membre** : fil d'Ariane, dossiers et fichiers avec taille, cases à cocher, bouton « Télécharger la sélection » indiquant le volume total.
  3. **Téléchargements** : file avec progression, débit, temps restant estimé, statut, actions.
  4. **Réglages** : nom, limites, créneaux, endpoints annoncés, état réseau (clé publique, adresse de tunnel, pairs WireGuard), relance du scan.
- Mise à jour en direct de la file et des statuts par Server-Sent Events (`/ui/api/events`).
- Thème clair et sombre selon `prefers-color-scheme`, utilisable sur mobile.

## Exigences non fonctionnelles

### Sécurité

- L'API pair-à-pair n'écoute que sur le netstack WireGuard. Elle n'est jamais liée à une interface de l'hôte.
- Chaque requête pair-à-pair passe par un middleware : IP source mappée à un membre non révoqué, sinon `403`. Les clés temporaires d'invitation n'accèdent qu'à `/join`.
- Chemins : nettoyage par `filepath.Clean`, rejet de `..` et des chemins absolus, résolution des liens symboliques puis vérification que le chemin réel reste sous `/share`. Utiliser `os.Root` (Go 1.24 et plus) si disponible.
- Les secrets (`node.key`, clé de groupe, jetons) ne sont jamais journalisés ni renvoyés par l'API UI, sauf le code d'invitation à sa création.
- Comparaisons de jetons et de HMAC en temps constant.
- Le HTTP en clair est acceptable dans le tunnel, puisque WireGuard chiffre et authentifie.
- Le conteneur tourne sans privilège, en utilisateur non root configurable (`user:` dans le compose), système de fichiers racine en lecture seule.

### Performance et ressources

- Cible : saturer un lien de 1 Gb/s dans le tunnel sur un NAS amd64 récent ; au moins 300 Mb/s sur un NAS arm64 d'entrée de gamme. Le netstack est le goulot probable : le mesurer dès le jalon 1.
- Mémoire au repos inférieure à 80 Mo ; image inférieure à 30 Mo.
- Index : un partage de 500 000 fichiers se scanne sans dépasser 200 Mo de mémoire. Lister un dossier répond en moins de 200 ms.
- Copies en flux : jamais de fichier entier en mémoire.

### Robustesse

- Arrêt propre sur `SIGTERM` : fin des écritures en cours, `.part` cohérents, base fermée. Délai maximal de 10 s.
- SQLite en mode WAL, écritures sérialisées.
- Un pair qui disparaît ne bloque ni l'interface ni les autres transferts : timeouts sur toutes les requêtes pair-à-pair (connexion 10 s, inactivité 60 s).
- Logs structurés (`log/slog`) sur stdout, niveau réglé par `LOG_LEVEL`.

### Compatibilité

- Images `linux/amd64` et `linux/arm64` obligatoires ; `linux/arm/v7` souhaitable pour les vieux NAS.
- Fonctionne avec Docker et Podman, et via l'interface Container Manager de Synology.

## Spécification technique

### Stack et dépendances

Go, dernière version stable, compilé avec `CGO_ENABLED=0`. Dépendances autorisées, toute autre à justifier :

| Besoin | Module |
| --- | --- |
| WireGuard userspace | `golang.zx2c4.com/wireguard` (`device`, `conn`, `tun/netstack`) |
| Clés WireGuard | `golang.zx2c4.com/wireguard/wgctrl/wgtypes` ou Curve25519 de `golang.org/x/crypto` |
| Limiteur de débit | `golang.org/x/time/rate` |
| SQLite sans cgo | `modernc.org/sqlite` |
| HTTP, JSON, logs, embed | bibliothèque standard (`net/http` et son routeur, `log/slog`, `embed`) |

### Structure du dépôt

```
cmd/cercle/          point d'entrée, sous-commandes serve et healthcheck
internal/config/     variables d'environnement et réglages en base
internal/wg/         wireguard-go, netstack, réconciliation des pairs
internal/membership/ cercle, invitations, fiches, gossip, HMAC
internal/index/      scan et requêtes sur le partage
internal/peerapi/    serveur et client HTTP pair-à-pair
internal/transfer/   file, workers, reprise, limiteurs
internal/store/      SQLite, migrations
internal/ui/         API UI, SSE, fichiers statiques embarqués
web/                 index.html, app.js, style.css
deploy/              Dockerfile, compose d'exemple, compose de test
```

### API pair-à-pair

HTTP/1.1 sur `<ip_tunnel>:8787`, JSON, préfixe `/api/v1`. Le `circle_id` est envoyé dans l'en-tête `X-Cercle-Circle` et vérifié.

| Méthode et route | Appelant | Rôle |
| --- | --- | --- |
| `POST /api/v1/join` | clé temporaire | Corps : jeton, clé publique réelle, nom. Réponse : fiche créée et table des membres. |
| `GET /api/v1/info` | membre | Nom, version du logiciel, taille et nombre de fichiers partagés, date du dernier scan. |
| `POST /api/v1/gossip` | membre | Corps : table de l'appelant. Réponse : table fusionnée du récepteur. |
| `GET /api/v1/files?path=&cursor=` | membre | Contenu d'un dossier, paginé par 500. |
| `GET /api/v1/tree?path=` | membre | Tous les fichiers sous un chemin, en NDJSON. |
| `GET /api/v1/file?path=` | membre | Contenu du fichier, `Range`, bride d'upload. |

### API de l'interface

HTTP sur `UI_LISTEN`, préfixe `/ui/api`, JSON.

| Méthode et route | Rôle |
| --- | --- |
| `GET /status` | État du nœud : dans un cercle ou non, identité, adresse de tunnel. |
| `POST /circle` | Créer un cercle. |
| `POST /join` | Rejoindre avec un code. |
| `GET /members` | Membres avec statut réseau. |
| `POST /invites`, `GET /invites`, `DELETE /invites/{id}` | Créer, lister, annuler des invitations. |
| `POST /members/{id}/revoke` | Révoquer un membre. |
| `GET /members/{id}/files?path=&cursor=` | Proxy vers le pair. |
| `POST /queue` | Corps : pair et liste de chemins (fichiers ou dossiers). |
| `GET /queue`, `PATCH /queue/{id}`, `DELETE /queue/{id}` | Lister et piloter la file. |
| `GET /settings`, `PUT /settings` | Réglages modifiables. |
| `POST /scan` | Relancer le scan. |
| `GET /events` | Flux SSE (progression, statuts). |

### Modèle de données

Une base `/data/cercle.db`, migrations versionnées et embarquées.

| Table | Colonnes principales |
| --- | --- |
| `circle` | `circle_id`, `group_key`, `subnet`, `created_at` |
| `members` | `node_id` (PK), `name`, `tunnel_ip`, `announced_endpoints` (JSON), `observed_endpoints` (JSON), `version`, `joined_at`, `updated_at`, `revoked`, `invited_by`, `hmac` |
| `invites` | `id`, `token_hash`, `temp_pubkey`, `assigned_ip`, `expires_at`, `used_at` |
| `files` | `path` (PK), `parent`, `is_dir`, `size`, `mtime`, `file_count`, `scan_gen` |
| `queue` | `id`, `peer_id`, `remote_path`, `dest_path`, `size`, `etag`, `status`, `bytes_done`, `priority`, `attempts`, `next_attempt_at`, `last_error`, `created_at`, `updated_at` |
| `settings` | `key`, `value` |

Statuts de file : `queued`, `active`, `paused`, `waiting` (pair indisponible, nouvel essai programmé), `done`, `error`.

### Configuration

| Variable | Défaut | Rôle |
| --- | --- | --- |
| `NODE_NAME` | nom d'hôte | Nom affiché aux autres membres |
| `WG_LISTEN_PORT` | `51820` | Port UDP WireGuard |
| `WG_PUBLIC_ENDPOINTS` | vide | Endpoints annoncés, séparés par des virgules |
| `TUNNEL_SUBNET` | `10.42.0.0/24` | Sous-réseau, utilisé à la création du cercle |
| `UI_LISTEN` | `:8080` | Adresse de l'interface |
| `UI_PASSWORD` | vide | Mot de passe de l'interface |
| `UPLOAD_LIMIT` | `0` | Débit d'upload maximal |
| `MAX_UPLOADS` | `3` | Uploads simultanés |
| `UPLOAD_WINDOW` | vide | Créneau d'upload |
| `DOWNLOAD_LIMIT` | `0` | Débit de téléchargement maximal |
| `DOWNLOAD_CONCURRENCY` | `2` | Téléchargements simultanés |
| `DOWNLOAD_WINDOW` | vide | Créneau de téléchargement |
| `SCAN_INTERVAL` | `1h` | Période de scan |
| `SHARE_EXCLUDE` | vide | Motifs glob exclus |
| `INVITE_TTL` | `72h` | Durée de validité d'une invitation |
| `TZ` | `UTC` | Fuseau pour les créneaux |
| `LOG_LEVEL` | `info` | Niveau de log |

## Packaging et distribution

### Image

- Dockerfile multi-stage : compilation dans l'image officielle `golang` (version épinglée), puis copie du binaire dans `scratch` avec les certificats CA et `zoneinfo` (ou `import _ "time/tzdata"`).
- `USER 65532:65532` par défaut, surchargé par `user:` dans le compose pour coller aux droits du NAS.
- `EXPOSE 51820/udp 8080/tcp`. Volumes : `/share` (lecture seule), `/downloads`, `/data`.
- `HEALTHCHECK` via la sous-commande `cercle healthcheck`, qui interroge l'interface en local (pas de `curl` dans `scratch`).
- Version, commit et date injectés par `-ldflags` et affichés dans l'interface et dans `/api/v1/info`.

### CI

- GitHub Actions : tests et `go vet` à chaque push ; build `buildx` multi-arch (`linux/amd64`, `linux/arm64`, `linux/arm/v7`) et publication sur `ghcr.io` à chaque tag `v*`. Tags d'image : version exacte, majeure.mineure, `latest`.
- Compilation croisée native Go (`GOARCH`), pas d'émulation QEMU pour la compilation.

### Compose fourni aux membres

```yaml
services:
  cercle:
    image: ghcr.io/<owner>/cercle:1
    container_name: cercle
    user: "1026:100"            # utilisateur du NAS propriétaire des dossiers
    read_only: true
    ports:
      - "51820:51820/udp"       # à rediriger sur la box
      - "8080:8080"             # interface web, LAN
    environment:
      NODE_NAME: alice
      WG_PUBLIC_ENDPOINTS: "alice.duckdns.org:51820"
      UI_PASSWORD: "change-moi"
      UPLOAD_LIMIT: 20M
      MAX_UPLOADS: 3
      TZ: Europe/Paris
    volumes:
      - /volume1/partage:/share:ro
      - /volume1/recu:/downloads
      - /volume1/docker/cercle:/data
    restart: unless-stopped
```

### Documentation

`README.md` en français : installation sur Synology, Unraid et Linux générique ; redirection du port UDP ; IPv6 ; création du cercle et invitation ; dépannage (pair jamais en ligne, droits de fichiers, débit faible).

## Tests et critères d'acceptation

### Stratégie de test

- **Unitaires** : nettoyage et confinement des chemins (traversée, liens symboliques), fusion des tables et règles de version, HMAC, allocation et conflits d'adresses, parsing de la configuration (débits, créneaux), logique de reprise (`.part`, ETag modifié), backoff.
- **Intégration en processus** : plusieurs nœuds complets dans un même test Go, chacun avec son WireGuard netstack sur un port UDP de `127.0.0.1`. Scénarios : création, invitation, propagation par gossip à un troisième nœud, navigation, téléchargement, coupure puis reprise, révocation.
- **Bout en bout** : `deploy/compose.test.yml` lance 5 conteneurs sur un réseau bridge Docker, avec un script qui enchaîne création, invitations, et un téléchargement croisé vérifié par somme de contrôle.
- **Banc de débit** : mesure du débit dans le tunnel entre deux nœuds, avec et sans `UPLOAD_LIMIT`. Les résultats sont notés dans le README.

### Critères d'acceptation de la v1

- [ ] Un nœud neuf crée un cercle en un clic et obtient `10.42.0.1`.
- [ ] Un second nœud rejoint avec un code d'invitation collé dans l'interface, sans aucune autre configuration.
- [ ] Un troisième nœud invité par le deuxième apparaît chez le premier en moins de 2 minutes, et les deux peuvent se parler directement.
- [ ] Un code déjà utilisé ou expiré est refusé avec un message clair.
- [ ] Un membre révoqué perd l'accès à tous les nœuds en moins de 2 minutes, y compris après redémarrage.
- [ ] Aucun port TCP de l'API pair-à-pair n'est joignable depuis l'hôte ou le LAN.
- [ ] Une requête `path=../../etc/passwd` ou via un lien symbolique sortant est refusée.
- [ ] Un dossier de 10 000 fichiers et 50 Go se met en file en un clic et se télécharge entièrement, avec des sommes de contrôle identiques.
- [ ] Arrêter le pair source en plein transfert puis le relancer reprend le fichier au bon octet, sans recommencer.
- [ ] Redémarrer le conteneur téléchargeur conserve la file et reprend les transferts.
- [ ] `UPLOAD_LIMIT=10M` maintient le débit sortant total entre 9 et 11 Mo/s avec 3 téléchargeurs simultanés.
- [ ] Le 4e téléchargeur avec `MAX_UPLOADS=3` attend puis démarre quand une place se libère.
- [ ] Les images amd64 et arm64 démarrent et passent le scénario bout en bout.
- [ ] L'image pèse moins de 30 Mo et le nœud au repos consomme moins de 80 Mo de mémoire.

## Jalons de livraison

Chaque jalon se termine par des tests verts, un commit et une démonstration possible. On ne passe au suivant qu'une fois le précédent validé.

1. **J1 : Tunnel.** Configuration, génération de clé, wireguard-go + netstack, deux nœuds reliés par une configuration statique de test, `GET /api/v1/info` à travers le tunnel, middleware d'identité. Banc de débit netstack. *Point de décision : si le débit est très en dessous des cibles, s'arrêter et en discuter.*
2. **J2 : Partage.** Store SQLite et migrations, scan et index, `files`, `tree`, `file` avec `Range`, limiteur d'upload, `MAX_UPLOADS`, confinement des chemins.
3. **J3 : Téléchargement.** File persistante, workers, reprise, backoff, créneaux, limite de téléchargement.
4. **J4 : Adhésion.** Création du cercle, invitations à clé temporaire, `join`, fiches et HMAC, gossip, réconciliation WireGuard, endpoints observés, conflits d'adresses, révocation. La configuration statique de J1 disparaît.
5. **J5 : Interface.** API UI, SSE, page unique avec les quatre vues, mot de passe.
6. **J6 : Livraison.** Dockerfile, healthcheck, CI multi-arch, compose de test à 5 nœuds, README.
7. **Après la v1.** Relais applicatif, recherche par nom, hash BLAKE3 optionnel pour vérifier l'intégrité, rotation de la clé de groupe.

## Questions ouvertes

- [ ] Combien de NAS du groupe peuvent ouvrir un port UDP (IPv4 redirigé ou IPv6) ? La réponse fixe la priorité du relais applicatif.
- [ ] Tout membre peut-il inviter et révoquer, ou faut-il un rôle d'administrateur ?
- [ ] Faut-il une rotation de la clé de groupe après une révocation ?
- [ ] Le nom du projet « Cercle » convient-il ?
- [ ] La recherche par nom dans le partage d'un pair est-elle nécessaire dès la v1 ?
- [ ] Faut-il partager plusieurs dossiers (plusieurs volumes `/share/<nom>`) plutôt qu'un seul ?

## Consignes pour l'agent de développement

Ce document est la source de vérité. Place-le à la racine du dépôt sous `PRD.md` et tiens un `CLAUDE.md` court qui y renvoie.

- Travaille jalon par jalon, dans l'ordre. Avant chaque jalon, écris un plan bref et attends une validation s'il s'écarte de ce document.
- Vérifie les API réelles des modules (`wireguard-go`, `netstack`, `modernc.org/sqlite`) dans leur code source ou leur documentation avant de les utiliser. Ne te fie pas à ta mémoire pour leurs signatures. Utilise la dernière version stable de Go et des modules.
- N'ajoute aucune dépendance hors de la liste autorisée sans le signaler et le justifier.
- Code idiomatique : `gofmt`, `go vet` et `staticcheck` propres ; erreurs enrichies avec `%w` ; `context` propagé partout ; aucune goroutine sans arrêt.
- Chaque jalon livre ses tests. Un critère d'acceptation non testé automatiquement est vérifié par un script dans `deploy/`.
- Commits petits et descriptifs en français, un ou plusieurs par jalon.
- Si une exigence se révèle irréalisable ou coûteuse (débit du netstack, comportement de `IpcSet`, NAT), arrête-toi, explique le problème et propose des options plutôt que de contourner en silence.
- À la fin de chaque jalon, mets à jour une section « État » du README : ce qui marche, ce qui manque, les écarts au PRD.
