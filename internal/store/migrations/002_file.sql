-- J3 : file de téléchargement. Un élément = un fichier distant.
CREATE TABLE queue (
    id              INTEGER PRIMARY KEY,
    peer_id         TEXT NOT NULL,              -- node_id du pair source
    peer_name       TEXT NOT NULL,              -- nom au moment de l'ajout
    remote_path     TEXT NOT NULL,
    dest_path       TEXT NOT NULL,              -- relatif à /downloads
    size            INTEGER NOT NULL,
    mtime           INTEGER NOT NULL,           -- nanosecondes Unix, appliqué à la fin
    etag            TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'active', 'paused', 'waiting', 'done', 'error')),
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    priority        INTEGER NOT NULL DEFAULT 0, -- 1 = priorité haute
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0, -- nanosecondes Unix
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX queue_pick ON queue (status, priority DESC, created_at, id);

-- Un même fichier distant n'est en file qu'une fois tant qu'il n'est pas terminé.
CREATE UNIQUE INDEX queue_pending ON queue (peer_id, remote_path) WHERE status <> 'done';

CREATE INDEX queue_peer_path ON queue (peer_id, remote_path);
