-- The menu and the promotions of the kitchen system, kept as versions: each change is a new row,
-- and the one with the highest number of its kind is the one in use. Nothing is updated or
-- deleted, so the history says who changed what and when, and an old version can be put back (as a
-- new one, after the others).
--
-- A document is the same JSON that the menu and promotions files hold, and is checked by the same
-- code before it is saved; this table does not know what is in it.

CREATE TABLE catalog_versions (
    kind       text        NOT NULL CHECK (kind IN ('menu', 'promotions')),
    version    integer     NOT NULL CHECK (version > 0),
    doc        jsonb       NOT NULL,
    created_at timestamptz NOT NULL,
    created_by text        NOT NULL,
    note       text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    PRIMARY KEY (kind, version)
);

CREATE FUNCTION catalog_versions_are_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'catalog_versions is append-only';
END
$$;

CREATE TRIGGER catalog_versions_no_change BEFORE UPDATE OR DELETE ON catalog_versions
    FOR EACH ROW EXECUTE FUNCTION catalog_versions_are_kept();
CREATE TRIGGER catalog_versions_no_truncate BEFORE TRUNCATE ON catalog_versions
    FOR EACH STATEMENT EXECUTE FUNCTION catalog_versions_are_kept();

-- The photo of an item. It is kept in the database, with the rest of the data of the venue, so
-- that a backup of the database is a backup of the menu; a photo is at most a megabyte.
CREATE TABLE item_images (
    item_id      text        PRIMARY KEY CHECK (item_id ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'),
    content_type text        NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png')),
    data         bytea       NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 1048576),
    sha256       text        NOT NULL CHECK (char_length(sha256) = 64),
    updated_at   timestamptz NOT NULL,
    updated_by   text        NOT NULL
);
