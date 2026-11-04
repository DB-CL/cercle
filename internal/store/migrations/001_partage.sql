-- J2 : index du partage et réglages.
CREATE TABLE files (
    path       TEXT PRIMARY KEY,           -- chemin relatif, séparé par des /, "" pour la racine
    parent     TEXT NOT NULL,              -- dossier parent ; la racine est son propre parent
    name       TEXT NOT NULL,              -- dernier élément, pour le tri
    is_dir     INTEGER NOT NULL,
    size       INTEGER NOT NULL,           -- taille, ou taille cumulée pour un dossier
    mtime      INTEGER NOT NULL,           -- nanosecondes Unix
    file_count INTEGER NOT NULL DEFAULT 0, -- fichiers sous un dossier, récursivement
    scan_gen   INTEGER NOT NULL
) STRICT;

-- Liste d'un dossier : dossiers d'abord, puis par nom sans tenir compte de la casse.
CREATE INDEX files_listing ON files (parent, is_dir DESC, name COLLATE NOCASE, name);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
