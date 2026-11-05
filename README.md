# Cercle

Cercle permet à un petit groupe d'amis (2 à 20 personnes) d'échanger de gros fichiers entre leurs NAS, allumés en permanence, sans service en ligne ni lien de transfert. Chacun lance le même conteneur Docker, parcourt depuis une page web les dossiers partagés des autres, coche ce qu'il veut, et les fichiers arrivent en arrière-plan sur son NAS.

Tout passe par un réseau WireGuard chiffré, intégré à l'application : rien d'autre que son port UDP n'est exposé sur Internet, et il n'y a rien à configurer à la main. Un code d'invitation suffit pour faire entrer un ami.

Spécification complète : [PRD.md](PRD.md).

## Ce qu'il faut

- Un NAS ou un serveur Linux avec Docker (Synology, QNAP, Unraid, TrueNAS, Debian…), en amd64, arm64 ou armv7.
- Un dossier à partager et un dossier où recevoir les fichiers.
- De préférence, **un port UDP ouvert sur la box** (redirection de port, voir plus bas). Ce n'est pas obligatoire pour tout le monde, mais dans chaque paire de membres, au moins l'un des deux doit être joignable. Le plus simple est que tous ouvrent leur port.
- Une adresse joignable depuis Internet : une adresse IP fixe, un nom DynDNS (par exemple DuckDNS) ou une adresse IPv6.

## Installation

Le fichier [`deploy/compose.yml`](deploy/compose.yml) sert de modèle. Les réglages à adapter :

| Réglage | À mettre |
| --- | --- |
| `user` | L'utilisateur et le groupe du NAS qui possèdent les dossiers, au format `uid:gid` (commande `id` sur le NAS). |
| `NODE_NAME` | Le nom que verront les autres membres. |
| `WG_PUBLIC_ENDPOINTS` | Adresse et port où les autres vous joignent : `alice.duckdns.org:51820`, `203.0.113.4:51820` ou `[2001:db8::1]:51820`. Plusieurs adresses séparées par des virgules. |
| `UI_PASSWORD` | Le mot de passe de l'interface web. |
| Volumes | `/share` (le dossier partagé, en lecture seule), `/downloads` (ce que vous recevez), `/data` (clés et base, à sauvegarder). |

### Synology (Container Manager)

1. Créez trois dossiers, par exemple `/volume1/partage` (existant), `/volume1/recu` et `/volume1/docker/cercle`.
2. Trouvez votre identifiant d'utilisateur : en SSH, `id votre-utilisateur` donne par exemple `uid=1026 gid=100`. Mettez `user: "1026:100"` dans le compose.
3. Container Manager, **Projet**, **Créer** : choisissez `/volume1/docker/cercle` comme chemin, « Créer un docker-compose.yml » et collez le modèle adapté.
4. Si le pare-feu de DSM est activé, autorisez le port UDP 51820 (Panneau de configuration, Sécurité, Pare-feu).
5. Ouvrez `http://<adresse-du-nas>:8080/`.

### Unraid

1. Installez le greffon **Compose Manager** depuis Community Applications, puis ajoutez une pile (« Add New Stack ») avec le modèle adapté.
2. Utilisez les chemins Unraid, par exemple `/mnt/user/partage:/share:ro`, `/mnt/user/recu:/downloads` et `/mnt/user/appdata/cercle:/data`.
3. `user: "99:100"` (nobody:users) convient aux partages Unraid par défaut.
4. Ouvrez `http://<adresse-du-serveur>:8080/`.

### Linux

```sh
mkdir -p ~/cercle && cd ~/cercle
curl -O https://raw.githubusercontent.com/DB-CL/cercle/main/deploy/compose.yml
# adapter user, NODE_NAME, WG_PUBLIC_ENDPOINTS, UI_PASSWORD et les volumes, puis :
docker compose up -d
```

Le dossier monté sur `/data` doit appartenir à l'utilisateur indiqué dans `user`. Par exemple : `sudo chown 1000:1000 /srv/cercle-data`.

### Ouvrir le port UDP sur la box

Dans l'interface de votre box, menu de redirection de ports (souvent appelé NAT, NAT/PAT ou « Gestion des ports ») : redirigez le port **UDP 51820** vers l'adresse locale du NAS, port 51820. C'est le seul port à ouvrir. L'interface web (8080) doit rester accessible uniquement depuis votre réseau local.

Si votre adresse IPv4 change, utilisez un nom DynDNS dans `WG_PUBLIC_ENDPOINTS`. Cercle le résout de nouveau toutes les 5 minutes.

Certaines offres, souvent en 4G ou 5G, partagent une même adresse IPv4 entre plusieurs abonnés (CGNAT) : la redirection y est alors impossible. Deux solutions :

- **IPv6** : si la box fournit de l'IPv6, ouvrez le port UDP 51820 vers le NAS dans son pare-feu IPv6 et annoncez l'adresse IPv6 du NAS : `WG_PUBLIC_ENDPOINTS: "[2001:db8::12]:51820"`.
- **Ne rien ouvrir** : vous pourrez quand même échanger avec les membres joignables, qui vous verront dès que vous les aurez contactés. En revanche, deux membres injoignables ne peuvent pas se parler directement.

## Premiers pas

1. **Le premier membre** ouvre l'interface et clique sur **Créer un cercle**.
2. Dans **Membres**, il clique sur **Inviter un ami**. Le code `cercle1:…` affiché sert une seule fois et expire au bout de 72 h : copiez-le et envoyez-le à votre ami par messagerie.
3. **L'ami** installe Cercle, ouvre son interface, colle le code dans **Rejoindre avec un code**, et c'est tout. N'importe quel membre peut ensuite inviter à son tour.
4. **Parcourir** un membre affiche ses dossiers. Cochez des fichiers ou des dossiers entiers, puis **Télécharger la sélection**. Les fichiers arrivent dans `/downloads/<nom du membre>/`, avec reprise automatique en cas de coupure.
5. Dans **Réglages**, chacun choisit ce qu'il accepte d'envoyer aux autres : débit maximal, nombre d'envois simultanés, et éventuellement un créneau horaire (par exemple `01:00-07:00`).

Un membre peut être **révoqué** par n'importe quel autre, depuis la page Membres. C'est définitif : il perd l'accès chez tout le monde en quelques secondes.

## Réglages

Tous se règlent par variables d'environnement. Ceux de la page Réglages (nom, débits, créneaux, nombre de transferts) y sont modifiables à chaud, et la valeur enregistrée prime ensuite sur l'environnement.

| Variable | Défaut | Rôle |
| --- | --- | --- |
| `NODE_NAME` | nom d'hôte | Nom affiché aux autres membres |
| `WG_LISTEN_PORT` | `51820` | Port UDP WireGuard |
| `WG_PUBLIC_ENDPOINTS` | vide | Adresses où vous joindre, séparées par des virgules |
| `TUNNEL_SUBNET` | `10.42.0.0/24` | Sous-réseau du cercle, choisi à sa création |
| `UI_LISTEN` | `:8080` | Adresse de l'interface |
| `UI_PASSWORD` | vide | Mot de passe de la page de connexion (fortement conseillé) |
| `UPLOAD_LIMIT` | `0` | Débit d'envoi maximal, au total ; suffixes K, M, G en puissances de 1024 (`20M` = 20 Mio/s), `0` = illimité |
| `MAX_UPLOADS` | `3` | Envois simultanés ; au-delà, les autres patientent |
| `UPLOAD_WINDOW` | vide | Créneau d'envoi quotidien, par exemple `01:00-07:00` |
| `DOWNLOAD_LIMIT` | `0` | Débit de réception maximal |
| `DOWNLOAD_CONCURRENCY` | `2` | Téléchargements simultanés (un seul à la fois par membre) |
| `DOWNLOAD_WINDOW` | vide | Créneau de téléchargement |
| `SCAN_INTERVAL` | `1h` | Fréquence d'indexation du dossier partagé |
| `SHARE_EXCLUDE` | vide | Motifs exclus du partage, par exemple `*.tmp,*.part` |
| `INVITE_TTL` | `72h` | Durée de validité d'un code d'invitation |
| `TZ` | `UTC` | Fuseau horaire des créneaux, par exemple `Europe/Paris` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error` |

Les fichiers et dossiers cachés (commençant par un point), `@eaDir`, `#recycle` et `$RECYCLE.BIN` ne sont jamais partagés.

## Dépannage

**Un membre n'est jamais « en ligne ».** Vérifiez que son port UDP est bien redirigé vers le NAS, et que son `WG_PUBLIC_ENDPOINTS` correspond à son adresse publique actuelle. La page Réglages, section Réseau, affiche les endpoints annoncés. Si les deux membres sont injoignables, ils ne peuvent pas se parler directement.

**« Permission denied » dans les journaux, ou fichiers reçus inaccessibles.** L'utilisateur `user:` du compose doit pouvoir lire `/share` et écrire dans `/downloads` et `/data`. Comparez avec `ls -ln` sur les dossiers du NAS.

**L'invitation est refusée : « ce nœud n'annonce aucune adresse joignable ».** Renseignez `WG_PUBLIC_ENDPOINTS`, ou faites inviter votre ami par un membre joignable.

**« Ce code d'invitation a déjà servi » ou « a expiré ».** Demandez un nouveau code : chacun ne sert qu'une fois.

**Le débit est faible.** Il est limité par la connexion montante de celui qui envoie, par ses réglages d'envoi (débit, créneau) et par la puissance de son NAS : un petit NAS arm64 chiffre moins vite qu'un serveur récent. Un seul fichier à la fois est transféré par membre.

**Le port 8080 est déjà pris.** Changez seulement le côté hôte dans le compose : `"8081:8080"`.

**Réinstaller sans perdre sa place dans le cercle.** Gardez le dossier `/data` : il contient les clés du nœud (`node.key`, `node.sign`) et la base. Sans lui, il faut une nouvelle invitation.

## Mise à jour

```sh
docker compose pull && docker compose up -d
```

L'image `ghcr.io/db-cl/cercle:1` suit les versions 1.x. La file de téléchargement et le cercle survivent aux redémarrages.

## Sécurité, en bref

- Rien n'est exposé sur Internet à part le port UDP de WireGuard. L'API entre nœuds n'écoute que dans le tunnel.
- Chaque nœud signe sa propre fiche (Ed25519) : personne ne peut se faire passer pour un autre membre, ni entrer sans invitation.
- Un membre ne voit que le dossier `/share` des autres, en lecture seule. Les chemins sont confinés à l'envoi comme à la réception.
- L'interface demande le mot de passe `UI_PASSWORD` dans sa propre page de connexion, puis garde une session (cookie signé, `HttpOnly`, `SameSite=Strict`, `Secure` en HTTPS) de 12 h, ou de 30 jours avec « Rester connecté ». Les essais ratés sont limités à un par seconde. Changer le mot de passe déconnecte toutes les sessions.
- Le conteneur tourne sans privilège, en utilisateur non root, avec un système de fichiers en lecture seule.

## Développement

Aucun Go n'est requis sur la machine : `deploy/go.sh` lance la chaîne Go dans Docker.

```sh
docker build --network host -f deploy/Dockerfile -t cercle .   # image (environ 15 Mo)
deploy/e2e.sh                                                   # 5 conteneurs, scénario complet
```

```sh
deploy/go.sh test -short ./...          # tests (sans le banc de débit)
deploy/go.sh vet ./...
deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@latest ./...
CGO_ENABLED=0 deploy/go.sh build -o /src/.bin/cercle ./cmd/cercle
```

### Essai à trois nœuds

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

### Banc de débit

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

Les six jalons de la v1 sont livrés. Il reste à valider sur le terrain, entre de vraies box, et à publier l'image (voir « Ce qui manque » ci-dessous).

### Critères d'acceptation de la v1

| Critère | Vérifié par | Résultat |
| --- | --- | --- |
| Un nœud neuf crée un cercle en un clic et obtient `10.42.0.1` | `internal/node`, `deploy/e2e.sh` | oui |
| Un second nœud rejoint avec un code collé, sans autre configuration | idem | oui |
| Un troisième, invité par le deuxième, apparaît chez le premier en moins de 2 min, liaison directe | idem | moins de 1 s |
| Un code déjà utilisé ou expiré est refusé avec un message clair | `internal/node`, `internal/ui` | oui |
| Un révoqué perd l'accès à tous les nœuds en moins de 2 min, y compris après redémarrage | `deploy/e2e.sh` | 5 s (premier essai de la sonde) |
| Aucun port TCP de l'API pair-à-pair joignable depuis l'hôte ou le LAN | conception (netstack), essais J1 | oui |
| `path=../../etc/passwd` ou lien symbolique sortant refusé | `internal/index`, `internal/peerapi` | oui |
| 10 000 fichiers en un clic, sommes de contrôle identiques | `internal/peerapi` (`CERCLE_QUEUE_FILES=10000`) | 10 001 fichiers en 1 min 53 ; 50 Go non testés ici |
| Source arrêtée en plein transfert : reprise au bon octet | `internal/transfer` | oui |
| Redémarrage du téléchargeur : file conservée, transferts repris | `internal/transfer` | oui |
| `UPLOAD_LIMIT=10M`, 3 téléchargeurs : 9 à 11 Mo/s au total | `internal/peerapi` | 10,07 Mio/s |
| 4e téléchargeur avec `MAX_UPLOADS=3` : attend puis démarre | `internal/peerapi`, `internal/transfer` | `503` + `Retry-After`, puis démarrage |
| Images amd64 et arm64 : démarrent et passent le bout en bout | `deploy/e2e.sh` ; CI sur runners amd64 et arm64 | amd64 : oui ; arm64 : construite, à exécuter en CI |
| Image de moins de 30 Mo, moins de 80 Mo de mémoire au repos | `deploy/e2e.sh` | 15 Mo ; 19 à 30 Mio |

### J6 : livraison (livré le 4 novembre 2026)

Ce qui marche :

- `deploy/Dockerfile` : compilation croisée dans `golang:1.27.3`, binaire seul dans `scratch` avec les certificats CA, fuseaux horaires embarqués. `USER 65532:65532`, `HEALTHCHECK` par `cercle healthcheck` (sans curl). Environ 15 Mo pour amd64, arm64 et arm/v7.
- `deploy/compose.yml` : modèle pour les membres (lecture seule, `tmpfs` sur `/tmp`).
- `deploy/compose.test.yml` et `deploy/e2e.sh` : 5 conteneurs sur un réseau bridge, adhésions en chaîne, téléchargements croisés vérifiés par `sha256sum`, révocation, redémarrage, taille de l'image et mémoire au repos.
- `.github/workflows/ci.yml` : format, `go vet`, `staticcheck`, tests, détecteur de concurrence ; bout en bout sur runners amd64 et arm64 natifs ; publication sur `ghcr.io` à chaque tag `v*` (version exacte, majeure.mineure, majeure, `latest`), sans émulation QEMU. Validé par `actionlint`.
- README pour les membres : installation Synology, Unraid et Linux, port UDP, IPv6 et CGNAT, premiers pas, réglages, dépannage.
- Au repos, le nœud rend au système la mémoire des tampons de transfert (sinon plus de 70 Mo pendant plusieurs minutes).

Ce qui manque :

- **Aucun essai entre deux vraies box** : la traversée de NAT et le débit réel restent à mesurer sur le terrain.
- **CI jamais exécutée** : le dépôt n'a pas encore de remote GitHub. Les runners arm64 natifs (`ubuntu-24.04-arm`) sont gratuits pour un dépôt public ; pour un dépôt privé, ce job demandera un plan payant ou devra être retiré.
- **Pas de mesure sur un NAS arm64** (débit du netstack, mémoire).
- Pas de test à 50 Go : la reprise et la vérification sont testées, mais pas sur un tel volume.

### J5 : interface (livré le 4 novembre 2026)

Ce qui marche :

- Page unique en français, embarquée dans le binaire (`web/` : HTML, CSS et JavaScript sans framework, sans étape de build, sans CDN). Thème clair ou sombre selon le système, utilisable sur mobile.
- Accueil hors cercle : « Créer un cercle » ou « Rejoindre avec un code ».
- **Membres** : en ligne ou non, adresse, taille partagée, dernière poignée de main, endpoint. Inviter (code à copier, invitations en attente annulables), révoquer, parcourir.
- **Partage d'un membre** : fil d'Ariane, dossiers d'abord, cases à cocher conservées pendant la navigation, barre « Télécharger la sélection » avec le volume total.
- **Téléchargements** : filtres par état avec compteurs ; progression, débit et temps restant en direct ; erreurs et prochain essai ; actions par élément ou sur une sélection ; vidage des terminés.
- **Réglages** : nom, débits, nombre de transferts et créneaux, appliqués à chaud, avec les erreurs affichées champ par champ. État du partage avec relance du scan. État réseau (adresse, port, endpoints annoncés, clé publique, cercle).
- API complète sous `/ui/api` : celle du PRD, plus `GET /share` et `POST /queue/actions` pour les actions groupées. Flux d'événements `GET /ui/api/events` : changements regroupés, et progression chaque seconde pendant un transfert.
- Sécurité : page de connexion par mot de passe (`UI_PASSWORD`) et session signée, avec un bandeau d'avertissement si aucun mot de passe n'est défini. L'authentification Basic reste acceptée pour les scripts, sans jamais déclencher la fenêtre du navigateur. En-tête `X-Cercle-UI` obligatoire pour toute modification (CSRF). CSP `default-src 'self'`. Aucun texte venu d'un pair n'est inséré comme HTML.

Vérifications : tests Go de l'API (parcours complet entre deux nœuds, mot de passe, CSRF, flux d'événements). Chaque vue contrôlée à l'œil dans Chromium sans écran, en clair et en sombre, sur ordinateur et sur mobile, sans erreur JavaScript.

Écarts au PRD :

- `POST /ui/api/queue/actions` et `GET /ui/api/share` ajoutés.
- La file s'affiche par pages de 200, filtrées par état : une file de 10 000 éléments reste fluide.

Ce qui manque :

- Pas de tri « naturel » des noms (IMG_2 avant IMG_10) : le tri reste alphabétique, sans tenir compte de la casse.
- Pas de test automatique du JavaScript lui-même, au-delà de la vérification de syntaxe.

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
- Début de l'API de l'interface (complétée en J5). `GET /healthz` reste ouvert pour le healthcheck.
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
