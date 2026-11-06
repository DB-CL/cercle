-- Circle, members, revocations, invitations.
CREATE TABLE circle (
    id         INTEGER PRIMARY KEY CHECK (id = 1), -- a single row
    circle_id  TEXT NOT NULL,
    subnet     TEXT NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

-- Each member's signed record, as received (JSON), plus columns useful for queries.
-- (The record column was first named "fiche"; migration 004 renames it.)
CREATE TABLE members (
    node_id    TEXT PRIMARY KEY,
    fiche      TEXT NOT NULL,
    tunnel_ip  TEXT NOT NULL,
    version    INTEGER NOT NULL,
    joined_at  INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- Signed revocations: final, one per revoked member.
CREATE TABLE revocations (
    target TEXT PRIMARY KEY,
    data   TEXT NOT NULL
) STRICT;

CREATE TABLE invites (
    id          TEXT PRIMARY KEY,
    token_hash  BLOB NOT NULL,
    temp_pubkey TEXT NOT NULL,
    temp_ip     TEXT NOT NULL,  -- address of the temporary key while joining
    assigned_ip TEXT NOT NULL,  -- final address of the new member
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER NOT NULL DEFAULT 0,
    used_by     TEXT NOT NULL DEFAULT '',
    response    TEXT NOT NULL DEFAULT '', -- /join response, replayed as is if the newcomer retries
    cancelled   INTEGER NOT NULL DEFAULT 0
) STRICT;
