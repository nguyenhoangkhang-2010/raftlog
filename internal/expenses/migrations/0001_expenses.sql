-- What the venue spends besides the goods it sells: rent, power, wages, repairs. A person writes each
-- down, with a category, an amount and the days it covers (one day, or a period, such as the month that
-- the rent is for, which a report spreads evenly over its days).
--
-- An entry is never changed or deleted. A mistake is voided, with a reason, and written again, so that the
-- books say what was written and what was taken back.

CREATE TABLE expenses (
    id          bigserial   PRIMARY KEY,
    category    text        NOT NULL CHECK (char_length(btrim(category)) BETWEEN 1 AND 40),
    amount      bigint      NOT NULL CHECK (amount > 0),
    from_date   date        NOT NULL,
    to_date     date        NOT NULL,
    note        text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    created_by  text        NOT NULL,
    created_at  timestamptz NOT NULL,
    voided_at   timestamptz,
    voided_by   text,
    void_reason text        NOT NULL DEFAULT '' CHECK (char_length(void_reason) <= 200),
    CHECK (to_date >= from_date AND to_date - from_date < 3660),
    CHECK ((voided_at IS NULL AND voided_by IS NULL) OR (voided_at IS NOT NULL AND voided_by IS NOT NULL))
);
CREATE INDEX expenses_by_days ON expenses (from_date, to_date) WHERE voided_at IS NULL;

CREATE FUNCTION expenses_are_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'expenses are kept';
    END IF;
    IF OLD.voided_at IS NOT NULL THEN
        RAISE EXCEPTION 'an expense that was voided cannot be changed';
    END IF;
    -- an entry changes in one way only: it is voided
    IF (to_jsonb(NEW) - 'voided_at' - 'voided_by' - 'void_reason') IS DISTINCT FROM (to_jsonb(OLD) - 'voided_at' - 'voided_by' - 'void_reason') THEN
        RAISE EXCEPTION 'an expense cannot be changed, only voided';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER expenses_no_delete BEFORE DELETE ON expenses FOR EACH ROW EXECUTE FUNCTION expenses_are_kept();
CREATE TRIGGER expenses_guard BEFORE UPDATE ON expenses FOR EACH ROW EXECUTE FUNCTION expenses_are_kept();
CREATE TRIGGER expenses_no_truncate BEFORE TRUNCATE ON expenses FOR EACH STATEMENT EXECUTE FUNCTION expenses_are_kept();
