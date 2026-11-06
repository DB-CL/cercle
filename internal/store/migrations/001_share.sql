-- Share index and settings.
CREATE TABLE files (
    path       TEXT PRIMARY KEY,           -- relative path, slash-separated, "" for the root
    parent     TEXT NOT NULL,              -- parent folder; the root is its own parent
    name       TEXT NOT NULL,              -- last element, for sorting
    is_dir     INTEGER NOT NULL,
    size       INTEGER NOT NULL,           -- size, or cumulated size for a folder
    mtime      INTEGER NOT NULL,           -- Unix nanoseconds
    file_count INTEGER NOT NULL DEFAULT 0, -- files under a folder, recursively
    scan_gen   INTEGER NOT NULL
) STRICT;

-- Folder listing: folders first, then by case-insensitive name.
CREATE INDEX files_listing ON files (parent, is_dir DESC, name COLLATE NOCASE, name);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
