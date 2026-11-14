-- Who works when, and when they worked: the schedule of the staff (the rota), the time clock, and the
-- hourly rates that turn hours into what labour cost.
--
-- A person is an account of the service, named here by its login. Nothing here is deleted. A planned shift
-- is cancelled, with a reason; a clock entry is corrected by a manager with a reason, and what it was before
-- stays in its history; a rate is changed by writing a later one.

CREATE TABLE rota (
    id           bigserial   PRIMARY KEY,
    user_id      text        NOT NULL CHECK (char_length(user_id) BETWEEN 1 AND 64),
    -- the day of the venue on which the shift starts; a shift may end on the next one
    day          date        NOT NULL,
    starts_at    timestamptz NOT NULL,
    ends_at      timestamptz NOT NULL,
    station      text        NOT NULL DEFAULT '' CHECK (char_length(station) <= 20),
    note         text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    status       text        NOT NULL CHECK (status IN ('planned', 'cancelled')),
    created_by   text        NOT NULL,
    created_at   timestamptz NOT NULL,
    updated_by   text        NOT NULL,
    updated_at   timestamptz NOT NULL,
    cancelled_by text,
    cancelled_at timestamptz,
    cancel_reason text       NOT NULL DEFAULT '' CHECK (char_length(cancel_reason) <= 200),
    CHECK (ends_at - starts_at BETWEEN interval '30 minutes' AND interval '16 hours'),
    CHECK ((status = 'planned' AND cancelled_at IS NULL) OR (status = 'cancelled' AND cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL))
);
CREATE INDEX rota_by_user ON rota (user_id, starts_at) WHERE status = 'planned';
CREATE INDEX rota_by_day ON rota (day);

CREATE TABLE time_entries (
    id        bigserial   PRIMARY KEY,
    user_id   text        NOT NULL CHECK (char_length(user_id) BETWEEN 1 AND 64),
    -- the day of the venue on which the entry started
    day       date        NOT NULL,
    clock_in  timestamptz NOT NULL,
    clock_out timestamptz,
    -- the planned shift that this entry was matched to when the person clocked in, if there was one
    rota_id   bigint      REFERENCES rota (id),
    -- written by a manager, for someone who forgot to clock
    manual    boolean     NOT NULL DEFAULT false,
    note      text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    in_by     text        NOT NULL,
    out_by    text,
    CHECK (clock_out IS NULL OR clock_out > clock_in),
    CHECK (clock_out IS NULL OR clock_out - clock_in <= interval '24 hours'),
    CHECK ((clock_out IS NULL AND out_by IS NULL) OR (clock_out IS NOT NULL AND out_by IS NOT NULL))
);
-- a person is clocked in once
CREATE UNIQUE INDEX one_open_entry_per_user ON time_entries (user_id) WHERE clock_out IS NULL;
CREATE INDEX time_entries_by_day ON time_entries (day, user_id);

-- What an entry was before a manager corrected it, and why.
CREATE TABLE time_entry_history (
    id             bigserial   PRIMARY KEY,
    entry_id       bigint      NOT NULL REFERENCES time_entries (id),
    changed_at     timestamptz NOT NULL,
    changed_by     text        NOT NULL,
    reason         text        NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 200),
    old_clock_in   timestamptz NOT NULL,
    old_clock_out  timestamptz,
    new_clock_in   timestamptz NOT NULL,
    new_clock_out  timestamptz
);
CREATE INDEX time_entry_history_by_entry ON time_entry_history (entry_id, id);

-- What an hour of a person's work costs, from a day on. The rate in force on a day is the latest one that
-- starts on or before it (and, of those, the one written last).
CREATE TABLE hourly_rates (
    id        bigserial   PRIMARY KEY,
    user_id   text        NOT NULL CHECK (char_length(user_id) BETWEEN 1 AND 64),
    from_date date        NOT NULL,
    rate      bigint      NOT NULL CHECK (rate BETWEEN 0 AND 10000000),
    set_by    text        NOT NULL,
    set_at    timestamptz NOT NULL
);
CREATE INDEX hourly_rates_by_user ON hourly_rates (user_id, from_date DESC, id DESC);

-- Nothing is deleted. (One function for the deletes of both tables; one each for what may not change,
-- because a function that reads a field of OLD fails on a table that has none.)
CREATE FUNCTION staff_rows_are_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is kept', TG_TABLE_NAME;
END
$$;

CREATE FUNCTION rota_may_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'cancelled' THEN
        RAISE EXCEPTION 'a cancelled shift cannot be changed';
    END IF;
    IF NEW.user_id <> OLD.user_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'who a shift is for, and who wrote it, cannot be changed';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION time_entry_may_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.user_id <> OLD.user_id OR NEW.in_by <> OLD.in_by OR NEW.manual <> OLD.manual THEN
        RAISE EXCEPTION 'whose entry it is cannot be changed';
    END IF;
    IF OLD.clock_out IS NOT NULL AND NEW.clock_out IS NULL THEN
        RAISE EXCEPTION 'an entry that was closed is not opened again';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER rota_guard BEFORE UPDATE ON rota FOR EACH ROW EXECUTE FUNCTION rota_may_change();
CREATE TRIGGER rota_no_delete BEFORE DELETE ON rota FOR EACH ROW EXECUTE FUNCTION staff_rows_are_kept();
CREATE TRIGGER rota_no_truncate BEFORE TRUNCATE ON rota FOR EACH STATEMENT EXECUTE FUNCTION staff_rows_are_kept();
CREATE TRIGGER time_entries_guard BEFORE UPDATE ON time_entries FOR EACH ROW EXECUTE FUNCTION time_entry_may_change();
CREATE TRIGGER time_entries_no_delete BEFORE DELETE ON time_entries FOR EACH ROW EXECUTE FUNCTION staff_rows_are_kept();
CREATE TRIGGER time_entries_no_truncate BEFORE TRUNCATE ON time_entries FOR EACH STATEMENT EXECUTE FUNCTION staff_rows_are_kept();

CREATE FUNCTION staff_log_is_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END
$$;
CREATE TRIGGER time_entry_history_no_change BEFORE UPDATE OR DELETE ON time_entry_history FOR EACH ROW EXECUTE FUNCTION staff_log_is_kept();
CREATE TRIGGER time_entry_history_no_truncate BEFORE TRUNCATE ON time_entry_history FOR EACH STATEMENT EXECUTE FUNCTION staff_log_is_kept();
CREATE TRIGGER hourly_rates_no_change BEFORE UPDATE OR DELETE ON hourly_rates FOR EACH ROW EXECUTE FUNCTION staff_log_is_kept();
CREATE TRIGGER hourly_rates_no_truncate BEFORE TRUNCATE ON hourly_rates FOR EACH STATEMENT EXECUTE FUNCTION staff_log_is_kept();
