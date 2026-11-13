// Package inventory keeps in PostgreSQL the warehouse data that is not a movement of the stock:
// the suppliers, the name and unit of each ingredient, and the stocktakes while they are counted.
//
// The quantities, the lots and their prices are events in the kitchen service's log. This package
// only holds what a person writes down about them: who the goods come from, what to call them, and
// what was counted. Applying a stocktake means writing its differences to the log; this package
// keeps the document and guards its states, so that a stocktake is applied once.
package inventory

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
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
const MigrationsTable = "inventory_migrations"

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
	ErrNotFound = errors.New("inventory: not found")
	ErrExists   = errors.New("inventory: that already exists")
	// ErrNotOpen says that the stocktake is not open: it is being applied, or done, or cancelled.
	ErrNotOpen = errors.New("inventory: the stocktake is not open")
	// ErrAnotherOpen says that a stocktake is open or being applied already.
	ErrAnotherOpen = errors.New("inventory: another stocktake is open")
)

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "inventory: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{fmt.Sprintf(format, args...)}
}

// Store is the warehouse data of a database.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a Store on a database whose migrations (see Migrations) have been applied. now is
// the clock; time.Now if nil.
func New(pool *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

func isViolation(err error, code string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == code
}

func text(s string, field string, lo, hi int) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < lo || n > hi {
		if lo == 0 {
			return "", invalid("%s is at most %d characters", field, hi)
		}
		return "", invalid("%s is %d to %d characters", field, lo, hi)
	}
	return s, nil
}

// ---- suppliers ------------------------------------------------------------------------

// Supplier is who goods come from.
type Supplier struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone,omitempty"`
	Note      string    `json:"note,omitempty"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

var supplierID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

func (s Supplier) check() (Supplier, error) {
	var err error
	if !supplierID.MatchString(s.ID) {
		return s, invalid("a supplier ID is 2 to 32 lower-case letters, digits, dashes and underscores")
	}
	if s.Name, err = text(s.Name, "the name", 1, 80); err != nil {
		return s, err
	}
	if s.Phone, err = text(s.Phone, "the phone", 0, 30); err != nil {
		return s, err
	}
	if s.Note, err = text(s.Note, "the note", 0, 200); err != nil {
		return s, err
	}
	return s, nil
}

const supplierColumns = `id, name, phone, note, active, created_at, created_by, updated_at, updated_by`

func scanSupplier(row pgx.Row) (Supplier, error) {
	var s Supplier
	err := row.Scan(&s.ID, &s.Name, &s.Phone, &s.Note, &s.Active, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt, &s.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Supplier{}, ErrNotFound
	}
	return s, err
}

// Suppliers lists the suppliers by name; the ones made inactive only if asked for.
func (s *Store) Suppliers(ctx context.Context, withInactive bool) ([]Supplier, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+supplierColumns+` FROM suppliers WHERE active OR $1 ORDER BY lower(name), id`, withInactive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Supplier
	for rows.Next() {
		sp, err := scanSupplier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// Supplier returns one supplier.
func (s *Store) Supplier(ctx context.Context, id string) (Supplier, error) {
	return scanSupplier(s.pool.QueryRow(ctx, `SELECT `+supplierColumns+` FROM suppliers WHERE id = $1`, id))
}

// CreateSupplier adds a supplier.
func (s *Store) CreateSupplier(ctx context.Context, sp Supplier, by string) (Supplier, error) {
	sp, err := sp.check()
	if err != nil {
		return Supplier{}, err
	}
	if by == "" {
		return Supplier{}, invalid("say who saves")
	}
	now := s.now()
	sp.Active, sp.CreatedAt, sp.CreatedBy, sp.UpdatedAt, sp.UpdatedBy = true, now, by, now, by
	_, err = s.pool.Exec(ctx, `INSERT INTO suppliers (`+supplierColumns+`) VALUES ($1,$2,$3,$4,true,$5,$6,$5,$6)`,
		sp.ID, sp.Name, sp.Phone, sp.Note, now, by)
	if isViolation(err, "23505") {
		return Supplier{}, ErrExists
	}
	return sp, err
}

// UpdateSupplier changes the name, phone and note of a supplier, and whether it is in use.
func (s *Store) UpdateSupplier(ctx context.Context, sp Supplier, by string) (Supplier, error) {
	sp, err := sp.check()
	if err != nil {
		return Supplier{}, err
	}
	if by == "" {
		return Supplier{}, invalid("say who saves")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE suppliers SET name=$2, phone=$3, note=$4, active=$5, updated_at=$6, updated_by=$7 WHERE id=$1`,
		sp.ID, sp.Name, sp.Phone, sp.Note, sp.Active, s.now(), by)
	if err != nil {
		return Supplier{}, err
	}
	if tag.RowsAffected() == 0 {
		return Supplier{}, ErrNotFound
	}
	return s.Supplier(ctx, sp.ID)
}

// ---- ingredients ----------------------------------------------------------------------

// Info is what is written down about an ingredient: its name, the unit that recipes and the stock
// count it in, and how many days it keeps.
type Info struct {
	Ingredient string `json:"ingredient"`
	Name       string `json:"name"`
	Unit       string `json:"unit"`
	// ShelfDays is 0 if not known.
	ShelfDays int       `json:"shelf_days,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// Infos returns what is known about the ingredients, by ingredient.
func (s *Store) Infos(ctx context.Context) (map[string]Info, error) {
	rows, err := s.pool.Query(ctx, `SELECT ingredient, name, unit, COALESCE(shelf_days, 0), updated_at, updated_by FROM ingredient_info`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Info{}
	for rows.Next() {
		var i Info
		if err := rows.Scan(&i.Ingredient, &i.Name, &i.Unit, &i.ShelfDays, &i.UpdatedAt, &i.UpdatedBy); err != nil {
			return nil, err
		}
		out[i.Ingredient] = i
	}
	return out, rows.Err()
}

// SaveInfo writes the name, unit and shelf life of an ingredient, adding or replacing them.
func (s *Store) SaveInfo(ctx context.Context, i Info, by string) (Info, error) {
	var err error
	if n := utf8.RuneCountInString(i.Ingredient); n < 1 || n > 40 || strings.TrimSpace(i.Ingredient) != i.Ingredient {
		return Info{}, invalid("an ingredient is 1 to 40 characters")
	}
	if i.Name, err = text(i.Name, "the name", 1, 80); err != nil {
		return Info{}, err
	}
	if i.Unit, err = text(i.Unit, "the unit", 1, 12); err != nil {
		return Info{}, err
	}
	if i.ShelfDays < 0 || i.ShelfDays > 3650 {
		return Info{}, invalid("a shelf life is from 1 to 3650 days, or empty")
	}
	if by == "" {
		return Info{}, invalid("say who saves")
	}
	i.UpdatedAt, i.UpdatedBy = s.now(), by
	var shelf any
	if i.ShelfDays > 0 {
		shelf = i.ShelfDays
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO ingredient_info (ingredient, name, unit, shelf_days, updated_at, updated_by) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (ingredient) DO UPDATE SET name=$2, unit=$3, shelf_days=$4, updated_at=$5, updated_by=$6`,
		i.Ingredient, i.Name, i.Unit, shelf, i.UpdatedAt, by)
	return i, err
}

// ---- stocktakes -----------------------------------------------------------------------

// The states of a stocktake.
const (
	Open      = "open"
	Applying  = "applying"
	Applied   = "applied"
	Cancelled = "cancelled"
)

// Line is what was found of one ingredient: Counted is the quantity counted, Expected what the
// stock said at that moment. The difference is what applying the stocktake writes to the log.
type Line struct {
	Ingredient string    `json:"ingredient"`
	Counted    int64     `json:"counted"`
	Expected   int64     `json:"expected"`
	CountedBy  string    `json:"counted_by"`
	CountedAt  time.Time `json:"counted_at"`
}

// Variance is the difference that was found: negative for what is missing.
func (l Line) Variance() int64 { return l.Counted - l.Expected }

// Stocktake is a count of the stock, over some time.
type Stocktake struct {
	ID       int64      `json:"id"`
	Status   string     `json:"status"`
	Note     string     `json:"note,omitempty"`
	OpenedBy string     `json:"opened_by"`
	OpenedAt time.Time  `json:"opened_at"`
	ClosedBy string     `json:"closed_by,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	Lines    []Line     `json:"lines,omitempty"`
}

const stocktakeColumns = `id, status, note, opened_by, opened_at, COALESCE(closed_by, ''), closed_at`

func scanStocktake(row pgx.Row) (Stocktake, error) {
	var t Stocktake
	err := row.Scan(&t.ID, &t.Status, &t.Note, &t.OpenedBy, &t.OpenedAt, &t.ClosedBy, &t.ClosedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stocktake{}, ErrNotFound
	}
	return t, err
}

// OpenStocktake starts a stocktake. Only one can be open or being applied at a time.
func (s *Store) OpenStocktake(ctx context.Context, by, note string) (Stocktake, error) {
	note, err := text(note, "the note", 0, 200)
	if err != nil {
		return Stocktake{}, err
	}
	if by == "" {
		return Stocktake{}, invalid("say who saves")
	}
	t, err := scanStocktake(s.pool.QueryRow(ctx, `INSERT INTO stocktakes (status, note, opened_by, opened_at) VALUES ('open', $1, $2, $3) RETURNING `+stocktakeColumns,
		note, by, s.now()))
	if isViolation(err, "23505") {
		return Stocktake{}, ErrAnotherOpen
	}
	return t, err
}

// Stocktake returns one stocktake with its lines, by ingredient.
func (s *Store) Stocktake(ctx context.Context, id int64) (Stocktake, error) {
	t, err := scanStocktake(s.pool.QueryRow(ctx, `SELECT `+stocktakeColumns+` FROM stocktakes WHERE id = $1`, id))
	if err != nil {
		return Stocktake{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT ingredient, counted, expected, counted_by, counted_at FROM stocktake_lines WHERE stocktake_id = $1 ORDER BY ingredient`, id)
	if err != nil {
		return Stocktake{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.Ingredient, &l.Counted, &l.Expected, &l.CountedBy, &l.CountedAt); err != nil {
			return Stocktake{}, err
		}
		t.Lines = append(t.Lines, l)
	}
	return t, rows.Err()
}

// Current returns the stocktake that is open or being applied, with its lines; ErrNotFound if none.
func (s *Store) Current(ctx context.Context) (Stocktake, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM stocktakes WHERE status IN ('open', 'applying')`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stocktake{}, ErrNotFound
	}
	if err != nil {
		return Stocktake{}, err
	}
	return s.Stocktake(ctx, id)
}

// Stocktakes lists the latest stocktakes, newest first, without their lines.
func (s *Store) Stocktakes(ctx context.Context, limit int) ([]Stocktake, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+stocktakeColumns+` FROM stocktakes ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stocktake
	for rows.Next() {
		t, err := scanStocktake(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Count writes what was found of an ingredient, replacing an earlier count of it. It is refused
// unless the stocktake is open.
func (s *Store) Count(ctx context.Context, id int64, l Line, by string) error {
	if l.Counted < 0 || l.Expected < 0 {
		return invalid("a quantity is not negative")
	}
	if n := utf8.RuneCountInString(l.Ingredient); n < 1 || n > 40 {
		return invalid("an ingredient is 1 to 40 characters")
	}
	if by == "" {
		return invalid("say who saves")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO stocktake_lines (stocktake_id, ingredient, counted, expected, counted_by, counted_at)
		SELECT $1, $2, $3, $4, $5, $6 WHERE EXISTS (SELECT 1 FROM stocktakes WHERE id = $1 AND status = 'open')
		ON CONFLICT (stocktake_id, ingredient) DO UPDATE SET counted=$3, expected=$4, counted_by=$5, counted_at=$6`,
		id, l.Ingredient, l.Counted, l.Expected, by, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return s.whyNotOpen(ctx, id)
	}
	return nil
}

// Uncount takes one line out of an open stocktake.
func (s *Store) Uncount(ctx context.Context, id int64, ingredient string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM stocktake_lines WHERE stocktake_id = $1 AND ingredient = $2
		AND EXISTS (SELECT 1 FROM stocktakes WHERE id = $1 AND status = 'open')`, id, ingredient)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return s.whyNotOpen(ctx, id)
	}
	return nil
}

func (s *Store) whyNotOpen(ctx context.Context, id int64) error {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT status FROM stocktakes WHERE id = $1`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if st != Open {
		return ErrNotOpen
	}
	return ErrNotFound // the line was not there
}

// BeginApply moves an open stocktake to "applying", so that its lines can no longer change, and
// returns it. A stocktake that is already being applied is returned again, so that an apply that
// was cut short can be finished.
func (s *Store) BeginApply(ctx context.Context, id int64, by string) (Stocktake, error) {
	if by == "" {
		return Stocktake{}, invalid("say who saves")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE stocktakes SET status = 'applying', closed_by = $2 WHERE id = $1 AND status = 'open'`, id, by); err != nil {
		return Stocktake{}, err
	}
	t, err := s.Stocktake(ctx, id)
	if err != nil {
		return Stocktake{}, err
	}
	if t.Status != Applying {
		return Stocktake{}, ErrNotOpen
	}
	return t, nil
}

// FinishApply marks a stocktake that was being applied as done.
func (s *Store) FinishApply(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE stocktakes SET status = 'applied', closed_at = $2 WHERE id = $1 AND status = 'applying'`, id, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotOpen
	}
	return nil
}

// CancelStocktake throws an open stocktake away; nothing was written to the log.
func (s *Store) CancelStocktake(ctx context.Context, id int64, by string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE stocktakes SET status = 'cancelled', closed_by = $2, closed_at = $3 WHERE id = $1 AND status = 'open'`, id, by, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return s.whyNotOpen(ctx, id)
	}
	return nil
}
