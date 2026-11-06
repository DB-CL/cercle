-- Download queue. One item = one remote file.
CREATE TABLE queue (
    id              INTEGER PRIMARY KEY,
    peer_id         TEXT NOT NULL,              -- node_id of the source peer
    peer_name       TEXT NOT NULL,              -- name when queued
    remote_path     TEXT NOT NULL,
    dest_path       TEXT NOT NULL,              -- relative to /downloads
    size            INTEGER NOT NULL,
    mtime           INTEGER NOT NULL,           -- Unix nanoseconds, applied at the end
    etag            TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'active', 'paused', 'waiting', 'done', 'error')),
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    priority        INTEGER NOT NULL DEFAULT 0, -- 1 = high priority
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0, -- Unix nanoseconds
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX queue_pick ON queue (status, priority DESC, created_at, id);

-- A remote file is queued only once until it is done.
CREATE UNIQUE INDEX queue_pending ON queue (peer_id, remote_path) WHERE status <> 'done';

CREATE INDEX queue_peer_path ON queue (peer_id, remote_path);
