-- J4 : cercle, membres, révocations, invitations.
CREATE TABLE circle (
    id         INTEGER PRIMARY KEY CHECK (id = 1), -- une seule ligne
    circle_id  TEXT NOT NULL,
    subnet     TEXT NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

-- Fiche signée de chaque membre, telle que reçue (JSON), plus les colonnes utiles aux requêtes.
CREATE TABLE members (
    node_id    TEXT PRIMARY KEY,
    fiche      TEXT NOT NULL,
    tunnel_ip  TEXT NOT NULL,
    version    INTEGER NOT NULL,
    joined_at  INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- Révocations signées : définitives, une par membre révoqué.
CREATE TABLE revocations (
    target TEXT PRIMARY KEY,
    data   TEXT NOT NULL
) STRICT;

CREATE TABLE invites (
    id          TEXT PRIMARY KEY,
    token_hash  BLOB NOT NULL,
    temp_pubkey TEXT NOT NULL,
    temp_ip     TEXT NOT NULL,  -- adresse de la clé temporaire, le temps de l'adhésion
    assigned_ip TEXT NOT NULL,  -- adresse définitive du nouveau membre
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER NOT NULL DEFAULT 0,
    used_by     TEXT NOT NULL DEFAULT '',
    response    TEXT NOT NULL DEFAULT '', -- réponse de /join, renvoyée telle quelle si le nouveau réessaie
    cancelled   INTEGER NOT NULL DEFAULT 0
) STRICT;
