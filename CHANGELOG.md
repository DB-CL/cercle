# Changements

## 1.0.0 (à publier)

Première version.

- Image Docker unique (amd64, arm64, armv7, environ 15 Mo), WireGuard embarqué en espace utilisateur : aucun privilège, aucun module noyau.
- Cercle créé en un clic, invitation par un code à usage unique, fiches signées Ed25519, gossip, révocation définitive.
- Partage en lecture seule des dossiers montés sous `/share`, choisis dans les réglages ; index incrémental (500 000 fichiers en 13 s).
- File de téléchargement persistante avec reprise, nouvelles tentatives, priorité, pause ; limites de débit et créneaux horaires dans les deux sens.
- Interface web en français, style terminal, page de connexion par mot de passe, mise à jour en direct.
- Durcissement : voir `SECURITY.md`.
