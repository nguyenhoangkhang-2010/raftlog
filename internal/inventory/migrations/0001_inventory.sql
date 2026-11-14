-- The warehouse data that is kept in rows and not in the log: who the goods come from, what each
-- ingredient is called and measured in, and the stocktakes while they are being counted.
--
-- The quantities of the stock, its lots and their prices are not here: they are events in the log
-- (a delivery, a write-off, an adjustment), with the ID of a supplier as a string. A supplier is
-- never deleted, only made inactive, so an ID in the log never points at nothing.

CREATE TABLE suppliers (
    id         text        PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{1,31}$'),
    name       text        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 80),
    phone      text        NOT NULL DEFAULT '' CHECK (char_length(phone) <= 30),
    note       text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL,
    created_by text        NOT NULL,
    updated_at timestamptz NOT NULL,
    updated_by text        NOT NULL
);

-- The name and the unit of an ingredient, and how long it keeps (to suggest a date when it
-- arrives). An ingredient that has no row here is shown by its ID, with no unit.
CREATE TABLE ingredient_info (
    ingredient text        PRIMARY KEY CHECK (char_length(ingredient) BETWEEN 1 AND 40),
    name       text        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 80),
    unit       text        NOT NULL CHECK (char_length(btrim(unit)) BETWEEN 1 AND 12),
    shelf_days integer     CHECK (shelf_days IS NULL OR shelf_days BETWEEN 1 AND 3650),
    updated_at timestamptz NOT NULL,
    updated_by text        NOT NULL
);

-- A stocktake is counted over some time and then applied: each line says what was found and what
-- the stock said when it was counted, and applying it writes the difference to the log. While one
-- is being applied (the log is written line by line) it cannot be changed, and it cannot be
-- applied twice. Only one is open at a time.
CREATE TABLE stocktakes (
    id         bigserial   PRIMARY KEY,
    status     text        NOT NULL CHECK (status IN ('open', 'applying', 'applied', 'cancelled')),
    note       text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    opened_by  text        NOT NULL,
    opened_at  timestamptz NOT NULL,
    closed_by  text,
    closed_at  timestamptz
);
CREATE UNIQUE INDEX one_stocktake_at_a_time ON stocktakes ((true)) WHERE status IN ('open', 'applying');

CREATE TABLE stocktake_lines (
    stocktake_id bigint      NOT NULL REFERENCES stocktakes (id),
    ingredient   text        NOT NULL CHECK (char_length(ingredient) BETWEEN 1 AND 40),
    counted      bigint      NOT NULL CHECK (counted >= 0),
    expected     bigint      NOT NULL CHECK (expected >= 0),
    counted_by   text        NOT NULL,
    counted_at   timestamptz NOT NULL,
    PRIMARY KEY (stocktake_id, ingredient)
);
