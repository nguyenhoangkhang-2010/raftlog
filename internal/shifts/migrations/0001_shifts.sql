-- The shifts of the cashiers and the cash in their drawers.
--
-- A shift is opened with a count of the drawer (how many notes of each kind) and closed with another.
-- What the drawer should hold is worked out at the close, from the opening count, the cash that
-- orders brought in and gave back (read from the log of the service by whoever closes) and the money
-- put in and taken out in between, and it is kept with the shift together with what it was made of,
-- so that a shift says later why it was short or over. A closed shift cannot be changed, except that a
-- manager may review it once; nothing is deleted.

CREATE TABLE shifts (
    id             bigserial   PRIMARY KEY,
    till           text        NOT NULL CHECK (till ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    cashier        text        NOT NULL CHECK (char_length(cashier) BETWEEN 1 AND 64),
    status         text        NOT NULL CHECK (status IN ('open', 'closed')),
    opened_at      timestamptz NOT NULL,
    opening_counts jsonb       NOT NULL,
    opening_total  bigint      NOT NULL CHECK (opening_total >= 0),

    closed_at      timestamptz,
    closed_by      text,
    closing_counts jsonb,
    closing_total  bigint      CHECK (closing_total >= 0),
    -- what the drawer should have held, and what it was made of
    sales          bigint      CHECK (sales >= 0),
    refunds        bigint      CHECK (refunds >= 0),
    pay_in         bigint      CHECK (pay_in >= 0),
    pay_out        bigint      CHECK (pay_out >= 0),
    expected       bigint,
    variance       bigint,
    close_note     text        NOT NULL DEFAULT '' CHECK (char_length(close_note) <= 300),

    reviewed_by    text,
    reviewed_at    timestamptz,
    review_note    text        NOT NULL DEFAULT '' CHECK (char_length(review_note) <= 300),

    CHECK ((status = 'open' AND closed_at IS NULL AND closed_by IS NULL AND closing_counts IS NULL AND closing_total IS NULL
                AND sales IS NULL AND refunds IS NULL AND pay_in IS NULL AND pay_out IS NULL AND expected IS NULL AND variance IS NULL
                AND reviewed_at IS NULL)
        OR (status = 'closed' AND closed_at IS NOT NULL AND closed_by IS NOT NULL AND closing_counts IS NOT NULL AND closing_total IS NOT NULL
                AND sales IS NOT NULL AND refunds IS NOT NULL AND pay_in IS NOT NULL AND pay_out IS NOT NULL AND expected IS NOT NULL AND variance IS NOT NULL
                AND closed_at >= opened_at))
);

-- A cashier has one open shift, and a drawer is in one open shift.
CREATE UNIQUE INDEX one_open_shift_per_cashier ON shifts (cashier) WHERE status = 'open';
CREATE UNIQUE INDEX one_open_shift_per_till ON shifts (till) WHERE status = 'open';
CREATE INDEX shifts_by_time ON shifts (opened_at DESC);

-- Money put into the drawer or taken out of it during a shift, such as a bank drop or ice that was
-- bought from the till.
CREATE TABLE shift_movements (
    id       bigserial   PRIMARY KEY,
    shift_id bigint      NOT NULL REFERENCES shifts (id),
    kind     text        NOT NULL CHECK (kind IN ('in', 'out')),
    amount   bigint      NOT NULL CHECK (amount > 0),
    reason   text        NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 200),
    by       text        NOT NULL,
    at       timestamptz NOT NULL
);
CREATE INDEX shift_movements_by_shift ON shift_movements (shift_id, id);

CREATE FUNCTION shifts_are_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'shifts are kept';
    END IF;
    IF OLD.status = 'closed' THEN
        -- a closed shift changes in one way only: it is reviewed, once
        IF (to_jsonb(NEW) - 'reviewed_by' - 'reviewed_at' - 'review_note') IS DISTINCT FROM (to_jsonb(OLD) - 'reviewed_by' - 'reviewed_at' - 'review_note') THEN
            RAISE EXCEPTION 'a closed shift cannot be changed';
        END IF;
        IF OLD.reviewed_at IS NOT NULL THEN
            RAISE EXCEPTION 'a shift is reviewed once';
        END IF;
    ELSIF OLD.status = 'open' AND NEW.status = 'open' AND (to_jsonb(NEW) <> to_jsonb(OLD)) THEN
        RAISE EXCEPTION 'an open shift changes only when it is closed';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER shifts_no_delete BEFORE DELETE ON shifts FOR EACH ROW EXECUTE FUNCTION shifts_are_kept();
CREATE TRIGGER shifts_guard BEFORE UPDATE ON shifts FOR EACH ROW EXECUTE FUNCTION shifts_are_kept();
CREATE TRIGGER shifts_no_truncate BEFORE TRUNCATE ON shifts FOR EACH STATEMENT EXECUTE FUNCTION shifts_are_kept();

CREATE FUNCTION shift_movements_are_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'shift_movements is append-only';
END
$$;
CREATE TRIGGER shift_movements_no_change BEFORE UPDATE OR DELETE ON shift_movements FOR EACH ROW EXECUTE FUNCTION shift_movements_are_kept();
CREATE TRIGGER shift_movements_no_truncate BEFORE TRUNCATE ON shift_movements FOR EACH STATEMENT EXECUTE FUNCTION shift_movements_are_kept();
