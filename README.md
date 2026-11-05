# Cercle

Cercle permet à un petit groupe d'amis (2 à 20 personnes) d'échanger de gros fichiers entre leurs NAS, allumés en permanence, sans service en ligne ni lien de transfert. Chacun lance le même conteneur Docker, parcourt depuis une page web les dossiers partagés des autres, coche ce qu'il veut, et les fichiers arrivent en arrière-plan sur son NAS.

Tout passe par un réseau WireGuard chiffré, intégré à l'application : rien d'autre que son port UDP n'est exposé sur Internet, et il n'y a rien à configurer à la main. Un code d'invitation suffit pour faire entrer un ami.

Projet libre : voir la section [Licence](#licence) ; pour contribuer, [docs/DEVELOPPEMENT.md](docs/DEVELOPPEMENT.md).

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
| Volumes | `/share` : ce que vous partagez, en lecture seule. Montez-y un ou plusieurs dossiers (`/mnt/nas/films:/share/films:ro`, `/mnt/nas/musique:/share/musique:ro`), puis choisissez dans Réglages lesquels partager. `/downloads` : ce que vous recevez. `/data` : clés et base, à sauvegarder. |

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
5. Dans **Réglages**, chacun choisit **quels dossiers partager** parmi ceux montés sous `/share` (par défaut : tous, y compris ceux montés plus tard), et ce qu'il accepte d'envoyer : débit maximal, nombre d'envois simultanés, et éventuellement un créneau horaire (par exemple `01:00-07:00`).

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
| `UI_PASSWORD` | vide | Mot de passe de la page de connexion ; vide = mot de passe aléatoire créé dans le fichier `ui.password` du volume `/data` |
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

Le code est en Go, sans dépendance système ; `deploy/go.sh` fournit la chaîne Go dans Docker pour qui ne l'a pas installée.

```sh
deploy/go.sh test -short ./...                                   # tests
docker build -f deploy/Dockerfile -t cercle .                    # image (environ 15 Mo)
deploy/e2e.sh                                                    # 5 conteneurs, scénario complet
```

Organisation du code, essais locaux et banc de débit : [docs/DEVELOPPEMENT.md](docs/DEVELOPPEMENT.md). Spécification d'origine : [docs/PRD.md](docs/PRD.md). Historique des jalons et des choix : [docs/JOURNAL.md](docs/JOURNAL.md). Changements : [CHANGELOG.md](CHANGELOG.md).

## État du projet

Version 1.0 : toutes les fonctions prévues sont là et testées, y compris par un scénario de bout en bout à 5 conteneurs. Elle n'a pas encore été éprouvée entre de vraies box (traversée de NAT, débit réel) ni mesurée sur un NAS arm64 : vos retours sont les bienvenus.

## Licence

Licence MIT : voir [LICENSE](LICENSE). Les composants tiers et leurs licences sont listés dans [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
