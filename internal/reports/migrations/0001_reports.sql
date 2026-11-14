-- The reports of the kitchen system: a copy of what happened, made by following the log of the
-- service and written to ordinary tables, so that a question about sales is a query.
--
-- Nothing here is the truth. The log is; these tables are built from it, can be thrown away and built
-- again (the service has a flag for it), and are never written by anything but the follower, which
-- writes a batch of the log and moves its checkpoint in the same transaction: a record is counted
-- once, however often the follower is stopped and started.

CREATE TABLE report_checkpoint (
    topic       text        PRIMARY KEY,
    -- the offset of the next record of the log that has not been counted
    next_offset bigint      NOT NULL CHECK (next_offset >= 0),
    -- the time of the latest event that was counted, which is how old the reports are
    as_of       timestamptz,
    -- records that could not be read and were passed over
    skipped     bigint      NOT NULL DEFAULT 0
);

CREATE TABLE orders (
    order_id     text        PRIMARY KEY,
    number       integer     NOT NULL,
    channel      text        NOT NULL,
    customer     text        NOT NULL DEFAULT '',
    placed_at    timestamptz NOT NULL,
    subtotal     bigint      NOT NULL,
    discount     bigint      NOT NULL,
    total        bigint      NOT NULL,
    status       text        NOT NULL,
    -- the sequence number of the last event counted for this order: an event is counted only if it is the next
    last_seq     integer     NOT NULL,
    -- what the whole order takes from the stock, by ingredient, as it was when the order was taken
    needs        jsonb       NOT NULL DEFAULT '{}',
    stock_held   boolean     NOT NULL DEFAULT false,
    -- the stations that make part of the order, and those that have finished theirs: the order is ready
    -- when they are all done
    stations       text[]      NOT NULL DEFAULT '{}',
    ready_stations text[]      NOT NULL DEFAULT '{}',
    paid_at      timestamptz,
    pay_method   text,
    paid_by      text,
    ready_at     timestamptz,
    completed_at timestamptz,
    cancelled_at timestamptz,
    refunded     bigint      NOT NULL DEFAULT 0
);
CREATE INDEX orders_by_paid_at ON orders (paid_at) WHERE paid_at IS NOT NULL;
CREATE INDEX orders_by_placed_at ON orders (placed_at);

CREATE TABLE order_lines (
    order_id text    NOT NULL REFERENCES orders (order_id) ON DELETE CASCADE,
    line_no  integer NOT NULL,
    kind     text    NOT NULL,
    ref      text    NOT NULL,
    name     text    NOT NULL,
    category text    NOT NULL,
    quantity integer NOT NULL,
    unit     bigint  NOT NULL,
    total    bigint  NOT NULL,
    PRIMARY KEY (order_id, line_no)
);
CREATE INDEX order_lines_by_ref ON order_lines (kind, ref);

-- Money given back, by a cancellation or by a refund after the order was completed. The method is
-- the way the order was paid, which is the way it is given back.
CREATE TABLE refunds (
    id       bigserial   PRIMARY KEY,
    order_id text        NOT NULL REFERENCES orders (order_id) ON DELETE CASCADE,
    at       timestamptz NOT NULL,
    kind     text        NOT NULL CHECK (kind IN ('cancel', 'refund')),
    amount   bigint      NOT NULL CHECK (amount > 0),
    method   text        NOT NULL DEFAULT '',
    by       text        NOT NULL,
    reason   text        NOT NULL DEFAULT ''
);
CREATE INDEX refunds_by_at ON refunds (at);

-- What orders took from the stock and gave back: positive for what an order took when it was paid,
-- negative for what a cancellation returned.
CREATE TABLE ingredient_usage (
    id         bigserial   PRIMARY KEY,
    order_id   text        NOT NULL REFERENCES orders (order_id) ON DELETE CASCADE,
    at         timestamptz NOT NULL,
    ingredient text        NOT NULL,
    qty        bigint      NOT NULL CHECK (qty <> 0)
);
CREATE INDEX ingredient_usage_by_at ON ingredient_usage (at, ingredient);

-- What people did to the stock: goods received, written off, counted, adjusted.
CREATE TABLE stock_movements (
    move_id    text        PRIMARY KEY,
    at         timestamptz NOT NULL,
    kind       text        NOT NULL,
    ingredient text        NOT NULL,
    qty        bigint      NOT NULL,
    cause      text        NOT NULL DEFAULT '',
    supplier   text        NOT NULL DEFAULT '',
    document   text        NOT NULL DEFAULT '',
    total_cost bigint      NOT NULL DEFAULT 0,
    by         text        NOT NULL,
    reason     text        NOT NULL DEFAULT ''
);
CREATE INDEX stock_movements_by_at ON stock_movements (at, ingredient);
