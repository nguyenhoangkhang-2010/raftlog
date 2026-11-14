// Package staff keeps in PostgreSQL who works when and when they worked: the schedule of the staff (the
// rota), the time clock, and the hourly rates that turn hours into the cost of labour.
//
// A person is an account of the service, named by its login; this package does not know accounts, and
// whoever calls it checks that the person is staff. The day is a day of the venue, read in the time zone
// the store was made with. Nothing is deleted: a shift is cancelled, a clock entry is corrected with a
// reason and keeps what it was, a rate is changed by writing a later one.
package staff

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationsTable is where the migrator records what it applied.
const MigrationsTable = "staff_migrations"

// Migrations returns the SQL files of this package, for the migrator.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

// Errors of the store.
var (
	ErrNotFound = errors.New("staff: not found")
	// ErrClockedIn says that the person is clocked in already.
	ErrClockedIn = errors.New("staff: already clocked in")
	// ErrNotClockedIn says that the person is not clocked in.
	ErrNotClockedIn = errors.New("staff: not clocked in")
	// ErrOverlap says that the shift or the entry overlaps another of the same person.
	ErrOverlap = errors.New("staff: that overlaps another shift or entry of the same person")
	// ErrCancelled says that the shift was cancelled.
	ErrCancelled = errors.New("staff: the shift was cancelled")
)

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "staff: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{fmt.Sprintf(format, args...)}
}

// Store is the staff data of a database.
type Store struct {
	pool *pgxpool.Pool
	loc  *time.Location
	now  func() time.Time
}

// New returns a Store on a database whose migrations (see Migrations) have been applied. loc is the time
// zone of the venue, in which a day starts and ends; now is the clock, time.Now if nil.
func New(pool *pgxpool.Pool, loc *time.Location, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.Local
	}
	return &Store{pool: pool, loc: loc, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

// Now is the clock of the store.
func (s *Store) Now() time.Time { return s.now() }

var (
	dayPattern  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	timePattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	userPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// hhmm reads a time like 08:30 that timePattern has accepted.
func hhmm(s string) (hours, minutes int) {
	hours, _ = strconv.Atoi(s[:2])
	minutes, _ = strconv.Atoi(s[3:])
	return hours, minutes
}

func (s *Store) day(t time.Time) string { return t.In(s.loc).Format("2006-01-02") }

func (s *Store) parseDay(d, what string) (time.Time, error) {
	if !dayPattern.MatchString(d) {
		return time.Time{}, invalid("%s is not a date like 2026-11-14", what)
	}
	t, err := time.ParseInLocation("2006-01-02", d, s.loc)
	if err != nil {
		return time.Time{}, invalid("%s is not a date like 2026-11-14", what)
	}
	return t, nil
}

func text(v, field string, limit int) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > limit {
		return "", invalid("%s is at most %d characters", field, limit)
	}
	return v, nil
}

func checkUser(u string) error {
	if !userPattern.MatchString(u) {
		return invalid("a person is named by a login: lower-case letters, digits, dots, dashes and underscores")
	}
	return nil
}

// lock serialises the changes to the shifts and entries of one person, so that two checks for an overlap
// cannot both pass and then both write.
func lock(ctx context.Context, tx pgx.Tx, user string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('staff:' || $1))`, user)
	return err
}

// ---- the schedule -----------------------------------------------------------------------

// Shift is a planned shift of work.
type Shift struct {
	ID    int64  `json:"id"`
	User  string `json:"user"`
	Day   string `json:"day"`
	Start string `json:"start"`
	End   string `json:"end"`
	// StartsAt and EndsAt are the moments; a shift whose end is not after its start by the clock runs
	// into the next day.
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       time.Time  `json:"ends_at"`
	Station      string     `json:"station,omitempty"`
	Note         string     `json:"note,omitempty"`
	Status       string     `json:"status"`
	CreatedBy    string     `json:"created_by"`
	UpdatedBy    string     `json:"updated_by"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CancelledBy  string     `json:"cancelled_by,omitempty"`
	CancelledAt  *time.Time `json:"cancelled_at,omitempty"`
	CancelReason string     `json:"cancel_reason,omitempty"`
}

// Status of a shift.
const (
	Planned   = "planned"
	Cancelled = "cancelled"
)

const shiftColumns = `id, user_id, day::text, starts_at, ends_at, station, note, status, created_by, updated_by, updated_at, COALESCE(cancelled_by, ''), cancelled_at, cancel_reason`

func (s *Store) scanShift(row pgx.Row) (Shift, error) {
	var sh Shift
	err := row.Scan(&sh.ID, &sh.User, &sh.Day, &sh.StartsAt, &sh.EndsAt, &sh.Station, &sh.Note, &sh.Status, &sh.CreatedBy, &sh.UpdatedBy, &sh.UpdatedAt, &sh.CancelledBy, &sh.CancelledAt, &sh.CancelReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shift{}, ErrNotFound
	}
	if err != nil {
		return Shift{}, err
	}
	sh.Start, sh.End = sh.StartsAt.In(s.loc).Format("15:04"), sh.EndsAt.In(s.loc).Format("15:04")
	return sh, nil
}

// moments turns a day and the times on the clock into the moments a shift starts and ends.
func (s *Store) moments(day, start, end string) (time.Time, time.Time, error) {
	d, err := s.parseDay(day, "the day")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !timePattern.MatchString(start) || !timePattern.MatchString(end) {
		return time.Time{}, time.Time{}, invalid("a time is like 08:30")
	}
	sh, sm := hhmm(start)
	eh, em := hhmm(end)
	from := time.Date(d.Year(), d.Month(), d.Day(), sh, sm, 0, 0, s.loc)
	to := time.Date(d.Year(), d.Month(), d.Day(), eh, em, 0, 0, s.loc)
	if !to.After(from) {
		to = time.Date(d.Year(), d.Month(), d.Day()+1, eh, em, 0, 0, s.loc) // into the next day
	}
	if to.Sub(from) < 30*time.Minute || to.Sub(from) > 16*time.Hour {
		return time.Time{}, time.Time{}, invalid("a shift is from 30 minutes to 16 hours long")
	}
	return from.UTC(), to.UTC(), nil
}

// ShiftInput is what is written for a shift.
type ShiftInput struct {
	User    string
	Day     string
	Start   string
	End     string
	Station string
	Note    string
}

func (s *Store) checkShift(in ShiftInput) (ShiftInput, time.Time, time.Time, error) {
	var err error
	if err = checkUser(in.User); err != nil {
		return in, time.Time{}, time.Time{}, err
	}
	if in.Station, err = text(in.Station, "the station", 20); err != nil {
		return in, time.Time{}, time.Time{}, err
	}
	if in.Note, err = text(in.Note, "the note", 200); err != nil {
		return in, time.Time{}, time.Time{}, err
	}
	from, to, err := s.moments(in.Day, in.Start, in.End)
	return in, from, to, err
}

// AddShift plans a shift for a person. It is refused if it overlaps another planned shift of theirs.
func (s *Store) AddShift(ctx context.Context, in ShiftInput, by string) (Shift, error) {
	if by == "" {
		return Shift{}, invalid("say who writes")
	}
	in, from, to, err := s.checkShift(in)
	if err != nil {
		return Shift{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Shift{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, in.User); err != nil {
		return Shift{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rota WHERE user_id = $1 AND status = 'planned' AND starts_at < $3 AND ends_at > $2`, in.User, from, to).Scan(&n); err != nil {
		return Shift{}, err
	}
	if n > 0 {
		return Shift{}, ErrOverlap
	}
	now := s.now()
	sh, err := s.scanShift(tx.QueryRow(ctx, `INSERT INTO rota (user_id, day, starts_at, ends_at, station, note, status, created_by, created_at, updated_by, updated_at)
		VALUES ($1, $2::text::date, $3, $4, $5, $6, 'planned', $7, $8, $7, $8) RETURNING `+shiftColumns, in.User, in.Day, from, to, in.Station, in.Note, by, now))
	if err != nil {
		return Shift{}, err
	}
	return sh, tx.Commit(ctx)
}

// UpdateShift changes a planned shift: the day, the times, the station and the note. Whom it is for does not
// change; cancel it and plan another.
func (s *Store) UpdateShift(ctx context.Context, id int64, in ShiftInput, by string) (Shift, error) {
	if by == "" {
		return Shift{}, invalid("say who writes")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Shift{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var user, status string
	if err := tx.QueryRow(ctx, `SELECT user_id, status FROM rota WHERE id = $1`, id).Scan(&user, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Shift{}, ErrNotFound
		}
		return Shift{}, err
	}
	if status != Planned {
		return Shift{}, ErrCancelled
	}
	in.User = user
	in, from, to, err := s.checkShift(in)
	if err != nil {
		return Shift{}, err
	}
	if err := lock(ctx, tx, user); err != nil {
		return Shift{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rota WHERE user_id = $1 AND status = 'planned' AND id <> $4 AND starts_at < $3 AND ends_at > $2`, user, from, to, id).Scan(&n); err != nil {
		return Shift{}, err
	}
	if n > 0 {
		return Shift{}, ErrOverlap
	}
	sh, err := s.scanShift(tx.QueryRow(ctx, `UPDATE rota SET day = $2::text::date, starts_at = $3, ends_at = $4, station = $5, note = $6, updated_by = $7, updated_at = $8
		WHERE id = $1 RETURNING `+shiftColumns, id, in.Day, from, to, in.Station, in.Note, by, s.now()))
	if err != nil {
		return Shift{}, err
	}
	return sh, tx.Commit(ctx)
}

// CancelShift takes a planned shift off the schedule, with a reason; it stays on record.
func (s *Store) CancelShift(ctx context.Context, id int64, by, reason string) (Shift, error) {
	if by == "" {
		return Shift{}, invalid("say who cancels")
	}
	reason, err := text(reason, "the reason", 200)
	if err != nil {
		return Shift{}, err
	}
	sh, err := s.scanShift(s.pool.QueryRow(ctx, `UPDATE rota SET status = 'cancelled', cancelled_by = $2, cancelled_at = $3, cancel_reason = $4, updated_by = $2, updated_at = $3
		WHERE id = $1 AND status = 'planned' RETURNING `+shiftColumns, id, by, s.now(), reason))
	if errors.Is(err, ErrNotFound) {
		var n int
		if qerr := s.pool.QueryRow(ctx, `SELECT count(*) FROM rota WHERE id = $1`, id).Scan(&n); qerr == nil && n > 0 {
			return Shift{}, ErrCancelled
		}
	}
	return sh, err
}

// Shifts lists the shifts that start on the days from `from` to `to`, both included, by time; the cancelled ones
// only if asked for. user may be empty for everyone.
func (s *Store) Shifts(ctx context.Context, from, to, user string, withCancelled bool) ([]Shift, error) {
	if _, err := s.parseDay(from, "the first day"); err != nil {
		return nil, err
	}
	if _, err := s.parseDay(to, "the last day"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+shiftColumns+` FROM rota WHERE day BETWEEN $1::text::date AND $2::text::date
		AND ($3 = '' OR user_id = $3) AND (status = 'planned' OR $4) ORDER BY starts_at, user_id, id LIMIT 2000`, from, to, user, withCancelled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Shift{}
	for rows.Next() {
		sh, err := s.scanShift(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// CopyDays plans again, `days` days later, the shifts of the days from `from` to `to`; a shift that would
// overlap one that is there already is left out. It returns how many it made and how many it left out.
func (s *Store) CopyDays(ctx context.Context, from, to string, days int, by string) (made, skipped int, err error) {
	if days < 1 || days > 366 {
		return 0, 0, invalid("a copy goes from 1 to 366 days on")
	}
	f, err := s.parseDay(from, "the first day")
	if err != nil {
		return 0, 0, err
	}
	t, err := s.parseDay(to, "the last day")
	if err != nil {
		return 0, 0, err
	}
	if t.Before(f) || t.Sub(f) > 31*24*time.Hour {
		return 0, 0, invalid("a copy covers from one to 32 days")
	}
	shifts, err := s.Shifts(ctx, from, to, "", false)
	if err != nil {
		return 0, 0, err
	}
	for _, sh := range shifts {
		d, _ := s.parseDay(sh.Day, "the day")
		in := ShiftInput{User: sh.User, Day: d.AddDate(0, 0, days).Format("2006-01-02"), Start: sh.Start, End: sh.End, Station: sh.Station, Note: sh.Note}
		if _, err := s.AddShift(ctx, in, by); err != nil {
			if errors.Is(err, ErrOverlap) {
				skipped++
				continue
			}
			return made, skipped, err
		}
		made++
	}
	return made, skipped, nil
}

// ---- the time clock ---------------------------------------------------------------------

// Entry is a stretch of work, from clocking in to clocking out.
type Entry struct {
	ID       int64      `json:"id"`
	User     string     `json:"user"`
	Day      string     `json:"day"`
	ClockIn  time.Time  `json:"clock_in"`
	ClockOut *time.Time `json:"clock_out,omitempty"`
	// Minutes is how long it was, whole minutes; 0 while the person is still clocked in.
	Minutes int64  `json:"minutes"`
	RotaID  *int64 `json:"rota_id,omitempty"`
	Manual  bool   `json:"manual,omitempty"`
	Note    string `json:"note,omitempty"`
	InBy    string `json:"in_by"`
	OutBy   string `json:"out_by,omitempty"`
	// Corrected counts how many times a manager changed it.
	Corrected int `json:"corrected,omitempty"`
	// In and Out are the times on the venue's clock, and OutDay the day it ended if that is not Day, for a
	// screen that edits them.
	In     string `json:"in"`
	Out    string `json:"out,omitempty"`
	OutDay string `json:"out_day,omitempty"`
}

const entryColumns = `e.id, e.user_id, e.day::text, e.clock_in, e.clock_out, COALESCE(floor(extract(epoch FROM e.clock_out - e.clock_in) / 60), 0)::bigint, e.rota_id, e.manual, e.note, e.in_by, COALESCE(e.out_by, ''),
    (SELECT count(*) FROM time_entry_history h WHERE h.entry_id = e.id)::int`

func (s *Store) scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	err := row.Scan(&e.ID, &e.User, &e.Day, &e.ClockIn, &e.ClockOut, &e.Minutes, &e.RotaID, &e.Manual, &e.Note, &e.InBy, &e.OutBy, &e.Corrected)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	e.In = e.ClockIn.In(s.loc).Format("15:04")
	if e.ClockOut != nil {
		e.Out = e.ClockOut.In(s.loc).Format("15:04")
		if od := s.day(*e.ClockOut); od != e.Day {
			e.OutDay = od
		}
	}
	return e, nil
}

// Span turns a day and the times on the venue's clock into the moments a stretch of work started and ended.
// An end that is not after the start is on the next day; an empty end leaves the stretch open, and is
// returned as nil.
func (s *Store) Span(day, in, out string) (time.Time, *time.Time, error) {
	d, err := s.parseDay(day, "the day")
	if err != nil {
		return time.Time{}, nil, err
	}
	if !timePattern.MatchString(in) || (out != "" && !timePattern.MatchString(out)) {
		return time.Time{}, nil, invalid("a time is like 08:30")
	}
	ih, im := hhmm(in)
	from := time.Date(d.Year(), d.Month(), d.Day(), ih, im, 0, 0, s.loc)
	if out == "" {
		return from.UTC(), nil, nil
	}
	oh, om := hhmm(out)
	to := time.Date(d.Year(), d.Month(), d.Day(), oh, om, 0, 0, s.loc)
	if !to.After(from) {
		to = time.Date(d.Year(), d.Month(), d.Day()+1, oh, om, 0, 0, s.loc)
	}
	u := to.UTC()
	return from.UTC(), &u, nil
}

// ClockIn starts a stretch of work for a person now. The entry is matched to the planned shift of theirs
// that is nearest to now, if one starts within three hours of it or is under way.
func (s *Store) ClockIn(ctx context.Context, user string) (Entry, error) {
	if err := checkUser(user); err != nil {
		return Entry{}, err
	}
	now := s.now()
	e, err := s.scanEntry(s.pool.QueryRow(ctx, `WITH r AS (
	    SELECT id FROM rota WHERE user_id = $1 AND status = 'planned' AND starts_at <= $2::timestamptz + interval '3 hours' AND ends_at > $2::timestamptz
	    ORDER BY abs(extract(epoch FROM starts_at - $2::timestamptz)), id LIMIT 1),
	ins AS (INSERT INTO time_entries (user_id, day, clock_in, rota_id, in_by) VALUES ($1, $3::text::date, $2, (SELECT id FROM r), $1) RETURNING *)
	SELECT `+entryColumns+` FROM ins e`, user, now, s.day(now)))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Entry{}, ErrClockedIn
	}
	return e, err
}

// ClockOut ends the stretch of work that the person has open, now.
func (s *Store) ClockOut(ctx context.Context, user string) (Entry, error) {
	if err := checkUser(user); err != nil {
		return Entry{}, err
	}
	e, err := s.scanEntry(s.pool.QueryRow(ctx, `WITH up AS (UPDATE time_entries SET clock_out = GREATEST($2::timestamptz, clock_in + interval '1 minute'), out_by = $1
	    WHERE user_id = $1 AND clock_out IS NULL RETURNING *)
	SELECT `+entryColumns+` FROM up e`, user, s.now()))
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, ErrNotFound):
		return Entry{}, ErrNotClockedIn
	case errors.As(err, &pe) && pe.Code == "23514":
		return Entry{}, invalid("that stretch of work would be longer than 24 hours: ask a manager to correct it")
	}
	return e, err
}

// OpenEntry returns the stretch of work that a person has open; ErrNotClockedIn if none.
func (s *Store) OpenEntry(ctx context.Context, user string) (Entry, error) {
	e, err := s.scanEntry(s.pool.QueryRow(ctx, `SELECT `+entryColumns+` FROM time_entries e WHERE e.user_id = $1 AND e.clock_out IS NULL`, user))
	if errors.Is(err, ErrNotFound) {
		return Entry{}, ErrNotClockedIn
	}
	return e, err
}

// OnDuty lists the people who are clocked in now, longest first.
func (s *Store) OnDuty(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+entryColumns+` FROM time_entries e WHERE e.clock_out IS NULL ORDER BY e.clock_in, e.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := s.scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Entries lists the stretches of work that started on the days from `from` to `to`, newest first. user may
// be empty for everyone.
func (s *Store) Entries(ctx context.Context, from, to, user string) ([]Entry, error) {
	if _, err := s.parseDay(from, "the first day"); err != nil {
		return nil, err
	}
	if _, err := s.parseDay(to, "the last day"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+entryColumns+` FROM time_entries e WHERE e.day BETWEEN $1::text::date AND $2::text::date AND ($3 = '' OR e.user_id = $3)
		ORDER BY e.clock_in DESC, e.id DESC LIMIT 2000`, from, to, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := s.scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns one entry.
func (s *Store) Get(ctx context.Context, id int64) (Entry, error) {
	return s.scanEntry(s.pool.QueryRow(ctx, `SELECT `+entryColumns+` FROM time_entries e WHERE e.id = $1`, id))
}

// overlaps says whether a stretch of work overlaps another of the same person; an entry that is open
// counts as lasting until the end of time.
func overlaps(ctx context.Context, tx pgx.Tx, user string, from time.Time, to *time.Time, except int64) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM time_entries WHERE user_id = $1 AND id <> $4 AND clock_in < COALESCE($3::timestamptz, 'infinity') AND COALESCE(clock_out, 'infinity') > $2`,
		user, from, to, except).Scan(&n)
	return n > 0, err
}

// AddEntry writes down a stretch of work that someone forgot to clock, with the reason.
func (s *Store) AddEntry(ctx context.Context, user string, in, out time.Time, reason, by string) (Entry, error) {
	if err := checkUser(user); err != nil {
		return Entry{}, err
	}
	if by == "" {
		return Entry{}, invalid("say who writes")
	}
	reason, err := text(reason, "the reason", 200)
	if err != nil {
		return Entry{}, err
	}
	if reason == "" {
		return Entry{}, invalid("say why")
	}
	if !out.After(in) || out.Sub(in) > 24*time.Hour {
		return Entry{}, invalid("a stretch of work ends after it starts and lasts at most 24 hours")
	}
	if out.After(s.now()) {
		return Entry{}, invalid("a stretch of work that is written down has ended already")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, user); err != nil {
		return Entry{}, err
	}
	if bad, err := overlaps(ctx, tx, user, in, &out, 0); err != nil {
		return Entry{}, err
	} else if bad {
		return Entry{}, ErrOverlap
	}
	e, err := s.scanEntry(tx.QueryRow(ctx, `WITH ins AS (INSERT INTO time_entries (user_id, day, clock_in, clock_out, manual, note, in_by, out_by) VALUES ($1, $2::text::date, $3, $4, true, $5, $6, $6) RETURNING *)
		SELECT `+entryColumns+` FROM ins e`, user, s.day(in), in, out, reason, by))
	if err != nil {
		return Entry{}, err
	}
	return e, tx.Commit(ctx)
}

// CorrectEntry changes when a stretch of work started and ended, with the reason; what it was is kept. out
// may be nil to leave a stretch open (it is only allowed for one that is open).
func (s *Store) CorrectEntry(ctx context.Context, id int64, in time.Time, out *time.Time, reason, by string) (Entry, error) {
	if by == "" {
		return Entry{}, invalid("say who corrects")
	}
	reason, err := text(reason, "the reason", 200)
	if err != nil {
		return Entry{}, err
	}
	if reason == "" {
		return Entry{}, invalid("say why")
	}
	if out != nil && (!out.After(in) || out.Sub(in) > 24*time.Hour) {
		return Entry{}, invalid("a stretch of work ends after it starts and lasts at most 24 hours")
	}
	if in.After(s.now()) || (out != nil && out.After(s.now())) {
		return Entry{}, invalid("a time that has not come yet cannot be written down")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var user string
	var oldIn time.Time
	var oldOut *time.Time
	if err := tx.QueryRow(ctx, `SELECT user_id, clock_in, clock_out FROM time_entries WHERE id = $1`, id).Scan(&user, &oldIn, &oldOut); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, err
	}
	if oldOut != nil && out == nil {
		return Entry{}, invalid("a stretch of work that ended is not opened again")
	}
	if err := lock(ctx, tx, user); err != nil {
		return Entry{}, err
	}
	if bad, err := overlaps(ctx, tx, user, in, out, id); err != nil {
		return Entry{}, err
	} else if bad {
		return Entry{}, ErrOverlap
	}
	if _, err := tx.Exec(ctx, `INSERT INTO time_entry_history (entry_id, changed_at, changed_by, reason, old_clock_in, old_clock_out, new_clock_in, new_clock_out) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, s.now(), by, reason, oldIn, oldOut, in, out); err != nil {
		return Entry{}, err
	}
	// whoever clocked the person out stays; a manager who closes an open entry is the one who clocked out
	var outBy *string
	if out != nil {
		outBy = &by
		if oldOut != nil {
			if err := tx.QueryRow(ctx, `SELECT out_by FROM time_entries WHERE id = $1`, id).Scan(&outBy); err != nil {
				return Entry{}, err
			}
		}
	}
	e, err := s.scanEntry(tx.QueryRow(ctx, `WITH up AS (UPDATE time_entries SET clock_in = $2, clock_out = $3, day = $4::text::date, out_by = $5 WHERE id = $1 RETURNING *)
		SELECT `+entryColumns+` FROM up e`, id, in, out, s.day(in), outBy))
	if err != nil {
		return Entry{}, err
	}
	return e, tx.Commit(ctx)
}

// Change is one correction of an entry.
type Change struct {
	At         time.Time  `json:"at"`
	By         string     `json:"by"`
	Reason     string     `json:"reason"`
	OldClockIn time.Time  `json:"old_clock_in"`
	OldOut     *time.Time `json:"old_clock_out,omitempty"`
	NewClockIn time.Time  `json:"new_clock_in"`
	NewOut     *time.Time `json:"new_clock_out,omitempty"`
	// Old and New say the same on the venue's clock, like "2026-11-10 08:00–12:00", for a screen to show.
	Old string `json:"old"`
	New string `json:"new"`
}

// spanText writes a stretch of work on the venue's clock: its day, and its times, with the day it ended on
// if that is another. A stretch that is still open ends in an ellipsis.
func (s *Store) spanText(in time.Time, out *time.Time) string {
	text := s.day(in) + " " + in.In(s.loc).Format("15:04") + "–"
	switch {
	case out == nil:
		return text + "…"
	case s.day(*out) != s.day(in):
		return text + s.day(*out) + " " + out.In(s.loc).Format("15:04")
	}
	return text + out.In(s.loc).Format("15:04")
}

// History lists the corrections of an entry, oldest first.
func (s *Store) History(ctx context.Context, id int64) ([]Change, error) {
	rows, err := s.pool.Query(ctx, `SELECT changed_at, changed_by, reason, old_clock_in, old_clock_out, new_clock_in, new_clock_out FROM time_entry_history WHERE entry_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.At, &c.By, &c.Reason, &c.OldClockIn, &c.OldOut, &c.NewClockIn, &c.NewOut); err != nil {
			return nil, err
		}
		c.Old, c.New = s.spanText(c.OldClockIn, c.OldOut), s.spanText(c.NewClockIn, c.NewOut)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- hours -----------------------------------------------------------------------------

// HoursRow is what one person planned and worked in a period.
type HoursRow struct {
	User          string `json:"user"`
	PlannedShifts int64  `json:"planned_shifts"`
	PlannedMin    int64  `json:"planned_minutes"`
	// WorkedMin is the time of the stretches of work that started in the period and ended; Days is
	// the days on which they started.
	WorkedMin int64 `json:"worked_minutes"`
	Days      int64 `json:"days"`
	// Late counts the planned shifts that began later than planned by more than five minutes (the
	// first entry of the shift counts) and LateMin how many minutes late in all; Missed counts the planned
	// shifts that are over and had no entry at all.
	Late    int64 `json:"late"`
	LateMin int64 `json:"late_minutes"`
	Missed  int64 `json:"missed"`
}

// Hours adds up, for each person who has a shift or an entry in the days from `from` to `to`, what was
// planned and what was worked. user may be empty for everyone.
func (s *Store) Hours(ctx context.Context, from, to, user string) ([]HoursRow, error) {
	if _, err := s.parseDay(from, "the first day"); err != nil {
		return nil, err
	}
	if _, err := s.parseDay(to, "the last day"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
WITH plan AS (
  SELECT r.id, r.user_id, r.starts_at, r.ends_at,
         floor(extract(epoch FROM r.ends_at - r.starts_at) / 60)::bigint AS minutes,
         (SELECT min(greatest(e.clock_in, r.starts_at)) FROM time_entries e
           WHERE e.user_id = r.user_id AND e.clock_in < r.ends_at AND COALESCE(e.clock_out, 'infinity'::timestamptz) > r.starts_at) AS first_in
  FROM rota r WHERE r.status = 'planned' AND r.day BETWEEN $1::text::date AND $2::text::date AND ($3 = '' OR r.user_id = $3)),
planned AS (
  SELECT user_id, count(*) AS shifts, sum(minutes) AS minutes,
         count(*) FILTER (WHERE first_in IS NOT NULL AND first_in > starts_at + interval '5 minutes') AS late,
         COALESCE(sum(floor(extract(epoch FROM first_in - starts_at) / 60)) FILTER (WHERE first_in IS NOT NULL AND first_in > starts_at + interval '5 minutes'), 0) AS late_min,
         count(*) FILTER (WHERE first_in IS NULL AND ends_at < $4) AS missed
  FROM plan GROUP BY user_id),
worked AS (
  SELECT user_id, COALESCE(sum(floor(extract(epoch FROM clock_out - clock_in) / 60)), 0) AS minutes, count(DISTINCT day) AS days
  FROM time_entries WHERE clock_out IS NOT NULL AND day BETWEEN $1::text::date AND $2::text::date AND ($3 = '' OR user_id = $3) GROUP BY user_id)
SELECT COALESCE(p.user_id, w.user_id), COALESCE(p.shifts, 0)::bigint, COALESCE(p.minutes, 0)::bigint, COALESCE(w.minutes, 0)::bigint, COALESCE(w.days, 0)::bigint,
       COALESCE(p.late, 0)::bigint, COALESCE(p.late_min, 0)::bigint, COALESCE(p.missed, 0)::bigint
FROM planned p FULL JOIN worked w ON w.user_id = p.user_id ORDER BY 1`, from, to, user, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HoursRow{}
	for rows.Next() {
		var r HoursRow
		if err := rows.Scan(&r.User, &r.PlannedShifts, &r.PlannedMin, &r.WorkedMin, &r.Days, &r.Late, &r.LateMin, &r.Missed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- rates and the cost of labour --------------------------------------------------------

// MaxRate bounds an hourly rate, in đồng.
const MaxRate = 10_000_000

// Rate is what an hour of a person's work costs from a day on.
type Rate struct {
	User  string    `json:"user"`
	From  string    `json:"from"`
	Rate  int64     `json:"rate"`
	SetBy string    `json:"set_by"`
	SetAt time.Time `json:"set_at"`
}

// SetRate writes the rate of a person from a day on. It does not change an earlier rate: the one in force on a
// day is the latest that starts on or before it.
func (s *Store) SetRate(ctx context.Context, user, from string, rate int64, by string) (Rate, error) {
	if err := checkUser(user); err != nil {
		return Rate{}, err
	}
	if by == "" {
		return Rate{}, invalid("say who writes")
	}
	if _, err := s.parseDay(from, "the day"); err != nil {
		return Rate{}, err
	}
	if rate < 0 || rate > MaxRate {
		return Rate{}, invalid("a rate is a whole number of đồng for an hour, up to %d", MaxRate)
	}
	r := Rate{User: user, From: from, Rate: rate, SetBy: by, SetAt: s.now()}
	_, err := s.pool.Exec(ctx, `INSERT INTO hourly_rates (user_id, from_date, rate, set_by, set_at) VALUES ($1, $2::text::date, $3, $4, $5)`, user, from, rate, by, r.SetAt)
	return r, err
}

// Rates returns the rate in force today for each person who has one, and the whole history for those who ask.
func (s *Store) Rates(ctx context.Context, withHistory bool) ([]Rate, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id, from_date::text, rate, set_by, set_at FROM (
	    SELECT *, row_number() OVER (PARTITION BY user_id ORDER BY from_date DESC, id DESC) AS k FROM hourly_rates WHERE $2 OR from_date <= $1::text::date) h
	WHERE $2 OR k = 1 ORDER BY user_id, from_date DESC, id DESC`, s.day(s.now()), withHistory)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rate{}
	for rows.Next() {
		var r Rate
		if err := rows.Scan(&r.User, &r.From, &r.Rate, &r.SetBy, &r.SetAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DayLabour is what the work that started on a day cost, in whole đồng, and how many minutes of it had no rate.
type DayLabour struct {
	Day        string `json:"day"`
	Cost       int64  `json:"cost"`
	Minutes    int64  `json:"minutes"`
	UnratedMin int64  `json:"unrated_minutes"`
}

// LabourByDay returns, for each day from `from` to `to` that has any, what the stretches of work that ended
// and started on it cost: the minutes times the rate in force that day, rounded once for the day. Work with no
// rate costs nothing and is counted in UnratedMin; a stretch that is still open is not counted.
func (s *Store) LabourByDay(ctx context.Context, from, to string) ([]DayLabour, error) {
	if _, err := s.parseDay(from, "the first day"); err != nil {
		return nil, err
	}
	if _, err := s.parseDay(to, "the last day"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
WITH w AS (
  SELECT e.day, floor(extract(epoch FROM e.clock_out - e.clock_in) / 60)::bigint AS minutes,
         (SELECT r.rate FROM hourly_rates r WHERE r.user_id = e.user_id AND r.from_date <= e.day ORDER BY r.from_date DESC, r.id DESC LIMIT 1) AS rate
  FROM time_entries e WHERE e.clock_out IS NOT NULL AND e.day BETWEEN $1::text::date AND $2::text::date)
SELECT day::text, round(COALESCE(sum(minutes * rate / 60.0), 0))::bigint, sum(minutes)::bigint, COALESCE(sum(minutes) FILTER (WHERE rate IS NULL), 0)::bigint
FROM w GROUP BY day ORDER BY day`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayLabour{}
	for rows.Next() {
		var d DayLabour
		if err := rows.Scan(&d.Day, &d.Cost, &d.Minutes, &d.UnratedMin); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Day is the day of the venue that a moment falls on, like 2026-11-14.
func (s *Store) Day(t time.Time) string { return s.day(t) }
