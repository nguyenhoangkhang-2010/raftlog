// Package reports keeps in PostgreSQL a copy of what happened in the kitchen system, built by
// following the log of the service, and answers the questions of a manager with queries over it:
// sales by day, what sold best, what the kitchen used, what was given back, when it was busy.
//
// The log is the truth and this is a view of it. The follower (in the service) reads the log in
// order and calls Apply with a batch; the batch and the checkpoint are written in one transaction,
// so a record is counted once however often the follower stops and starts. The package does not
// read the log and knows nothing of its format: the follower turns each record into calls on a Tx.
package reports

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationsTable is where the migrator records what it applied.
const MigrationsTable = "reports_migrations"

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
	// ErrOutOfStep says that the checkpoint is not where the batch starts: another follower wrote,
	// or the batch is old. Nothing was written.
	ErrOutOfStep = errors.New("reports: the checkpoint is not where this batch starts")
)

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "reports: " + e.Reason }

// Store is the reports of a database.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store on a database whose migrations (see Migrations) have been applied.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Checkpoint says how far the follower has read the log of a topic.
type Checkpoint struct {
	// Next is the offset of the next record that has not been counted.
	Next uint64 `json:"next_offset"`
	// AsOf is the time of the latest event that was counted; nil before any.
	AsOf    *time.Time `json:"as_of,omitempty"`
	Skipped int64      `json:"skipped"`
}

// Checkpoint returns where the follower is; zero if it never started.
func (s *Store) Checkpoint(ctx context.Context, topic string) (Checkpoint, error) {
	var c Checkpoint
	var next int64
	err := s.pool.QueryRow(ctx, `SELECT next_offset, as_of, skipped FROM report_checkpoint WHERE topic = $1`, topic).Scan(&next, &c.AsOf, &c.Skipped)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, nil
	}
	c.Next = uint64(next)
	return c, err
}

// Reset throws away every fact and the checkpoint of a topic, so that the follower reads the log
// again from the start.
func (s *Store) Reset(ctx context.Context, topic string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`DELETE FROM stock_movements`, `DELETE FROM orders`, `DELETE FROM report_checkpoint WHERE topic = $1`} {
		var args []any
		if q == `DELETE FROM report_checkpoint WHERE topic = $1` {
			args = append(args, topic)
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Apply runs fn on a transaction that holds the checkpoint, and moves the checkpoint from `from` to
// `next` in the same transaction. If the checkpoint is not at `from`, nothing is written and it is
// ErrOutOfStep. fn says, through Tx.Skipped and Tx.Seen, how many records of the batch could not be
// read and the time of the latest event in it; both are kept with the checkpoint.
func (s *Store) Apply(ctx context.Context, topic string, from, next uint64, fn func(*Tx) error) error {
	if next < from {
		return &InvalidError{"a batch does not end before it starts"}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO report_checkpoint (topic, next_offset) VALUES ($1, 0) ON CONFLICT DO NOTHING`, topic); err != nil {
		return err
	}
	var at int64
	if err := tx.QueryRow(ctx, `SELECT next_offset FROM report_checkpoint WHERE topic = $1 FOR UPDATE`, topic).Scan(&at); err != nil {
		return err
	}
	if uint64(at) != from {
		return ErrOutOfStep
	}
	t := &Tx{tx: tx, ctx: ctx}
	if err := fn(t); err != nil {
		return err
	}
	var as any
	if !t.asOf.IsZero() {
		as = t.asOf
	}
	if _, err := tx.Exec(ctx, `UPDATE report_checkpoint SET next_offset = $2, skipped = skipped + $3, as_of = GREATEST(as_of, $4::timestamptz) WHERE topic = $1`,
		topic, int64(next), t.skipped, as); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- the facts ------------------------------------------------------------------------

// Tx writes the facts of a batch.
type Tx struct {
	tx      pgx.Tx
	ctx     context.Context
	skipped int
	asOf    time.Time
}

// Skipped says that a record of the batch could not be read and was passed over.
func (t *Tx) Skipped() { t.skipped++ }

// Seen says that the batch holds an event of that time, which is how old the reports are.
func (t *Tx) Seen(at time.Time) {
	if at.After(t.asOf) {
		t.asOf = at
	}
}

// Line is a line of an order as it was taken.
type Line struct {
	Kind, Ref, Name, Category string
	Quantity                  int
	Unit, Total               int64
}

// Usage is what an order took from the stock (positive) or gave back (negative), and what it was
// worth at the price of the lots it came from, in thousandths of a đồng (negative with the quantity).
func (t *Tx) Usage(orderID string, at time.Time, ingredient string, qty, valueMilli int64) error {
	if qty == 0 {
		return nil
	}
	_, err := t.tx.Exec(t.ctx, `INSERT INTO ingredient_usage (order_id, at, ingredient, qty, value_milli)
		SELECT $1, $2, $3, $4, $5 WHERE EXISTS (SELECT 1 FROM orders WHERE order_id = $1)`, orderID, at, ingredient, qty, valueMilli)
	return err
}

// PlacedOrder is an order as it was taken.
type PlacedOrder struct {
	ID       string
	Number   int
	Channel  string
	Customer string
	At       time.Time
	Subtotal int64
	Discount int64
	Total    int64
	Lines    []Line
	// Stations are the stations that make part of the order.
	Stations []string
}

// OrderPlaced counts an order that was taken. An order that is counted already is left as it is.
func (t *Tx) OrderPlaced(o PlacedOrder) error {
	if o.Stations == nil {
		o.Stations = []string{}
	}
	tag, err := t.tx.Exec(t.ctx, `INSERT INTO orders (order_id, number, channel, customer, placed_at, subtotal, discount, total, status, last_seq, stations)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'placed', 1, $9) ON CONFLICT (order_id) DO NOTHING`,
		o.ID, o.Number, o.Channel, o.Customer, o.At, o.Subtotal, o.Discount, o.Total, o.Stations)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	for i, l := range o.Lines {
		if _, err := t.tx.Exec(t.ctx, `INSERT INTO order_lines (order_id, line_no, kind, ref, name, category, quantity, unit, total) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			o.ID, i+1, l.Kind, l.Ref, l.Name, l.Category, l.Quantity, l.Unit, l.Total); err != nil {
			return err
		}
	}
	return nil
}

// next moves an order to its next event and says whether it was: an event that is not the next one
// of its order (the same event again, or one of an order that was not counted) changes nothing.
func (t *Tx) next(id string, seq int, set string, args ...any) (bool, error) {
	all := append([]any{id, seq}, args...)
	tag, err := t.tx.Exec(t.ctx, `UPDATE orders SET last_seq = $2`+set+` WHERE order_id = $1 AND last_seq = $2 - 1`, all...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// OrderTouched counts an event that changes nothing that a report shows (a cancellation was asked for
// or refused).
func (t *Tx) OrderTouched(id string, seq int) error {
	_, err := t.next(id, seq, ``)
	return err
}

// OrderStarted records that a station began its part of an order.
func (t *Tx) OrderStarted(id string, seq int) error {
	_, err := t.next(id, seq, `, status = CASE WHEN status = 'paid' THEN 'preparing' ELSE status END`)
	return err
}

// OrderStationReady records that a station finished its part. The order is ready when every station
// that makes part of it has finished.
func (t *Tx) OrderStationReady(id string, seq int, station string, at time.Time) error {
	_, err := t.next(id, seq, `, ready_stations = CASE WHEN $3::text = ANY(ready_stations) THEN ready_stations ELSE array_append(ready_stations, $3::text) END,
		status = CASE WHEN status IN ('paid', 'preparing') AND cardinality(CASE WHEN $3::text = ANY(ready_stations) THEN ready_stations ELSE array_append(ready_stations, $3::text) END) >= cardinality(stations) THEN 'ready' ELSE status END,
		ready_at = CASE WHEN ready_at IS NULL AND cardinality(CASE WHEN $3::text = ANY(ready_stations) THEN ready_stations ELSE array_append(ready_stations, $3::text) END) >= cardinality(stations) THEN $4::timestamptz ELSE ready_at END`, station, at)
	return err
}

// OrderHandedOver records that the order was given to the customer.
func (t *Tx) OrderHandedOver(id string, seq int, at time.Time) error {
	_, err := t.next(id, seq, `, status = 'completed', completed_at = $3`, at)
	return err
}

// OrderPaid records a payment.
func (t *Tx) OrderPaid(id string, seq int, at time.Time, by, method string) error {
	_, err := t.next(id, seq, `, status = 'paid', paid_at = $3, paid_by = $4, pay_method = $5`, at, by, method)
	return err
}

// OrderCancelled records a cancellation and the money given back with it.
func (t *Tx) OrderCancelled(id string, seq int, at time.Time, by, reason string, refund int64) error {
	var method *string
	if err := t.tx.QueryRow(t.ctx, `SELECT pay_method FROM orders WHERE order_id = $1`, id).Scan(&method); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // an order that was not counted
		}
		return err
	}
	ok, err := t.next(id, seq, `, status = 'cancelled', cancelled_at = $3, refunded = refunded + $4`, at, refund)
	if err != nil || !ok {
		return err
	}
	if refund > 0 {
		return t.refund(id, at, "cancel", refund, method, by, reason)
	}
	return nil
}

// OrderRefunded records money given back after the order was completed.
func (t *Tx) OrderRefunded(id string, seq int, at time.Time, by, reason string, amount int64) error {
	var method *string
	if err := t.tx.QueryRow(t.ctx, `SELECT pay_method FROM orders WHERE order_id = $1`, id).Scan(&method); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	ok, err := t.next(id, seq, `, refunded = refunded + $3`, amount)
	if err != nil || !ok {
		return err
	}
	return t.refund(id, at, "refund", amount, method, by, reason)
}

func (t *Tx) refund(id string, at time.Time, kind string, amount int64, method *string, by, reason string) error {
	m := ""
	if method != nil {
		m = *method
	}
	_, err := t.tx.Exec(t.ctx, `INSERT INTO refunds (order_id, at, kind, amount, method, by, reason) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, at, kind, amount, m, by, reason)
	return err
}

// StockMovement is something a person did to the stock.
type StockMovement struct {
	ID         string
	At         time.Time
	Kind       string
	Ingredient string
	Qty        int64
	Cause      string
	Supplier   string
	Document   string
	TotalCost  int64
	By         string
	Reason     string
	// Delta is how much the quantity changed (negative for what left) and ValueMilli what that was
	// worth at the price of the lots it touched, in thousandths of a đồng.
	Delta      int64
	ValueMilli int64
}

// StockMoved counts a movement of the stock. One that is counted already is left as it is.
func (t *Tx) StockMoved(m StockMovement) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO stock_movements (move_id, at, kind, ingredient, qty, cause, supplier, document, total_cost, by, reason, delta, value_milli)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (move_id) DO NOTHING`,
		m.ID, m.At, m.Kind, m.Ingredient, m.Qty, m.Cause, m.Supplier, m.Document, m.TotalCost, m.By, m.Reason, m.Delta, m.ValueMilli)
	return err
}

// ---- the questions --------------------------------------------------------------------

// Period is a range of days, both included, read in a time zone.
type Period struct {
	From, To string // 2026-11-01
	Zone     string // Asia/Ho_Chi_Minh
}

var dayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// MaxDays is the longest period a report covers.
const MaxDays = 366

// check refuses a period that is not one, before it reaches the database.
func (p Period) check() error {
	loc, err := time.LoadLocation(p.Zone)
	if err != nil || p.Zone == "" {
		return &InvalidError{fmt.Sprintf("%q is not a time zone", p.Zone)}
	}
	var from, to time.Time
	for _, d := range []struct {
		s   string
		dst *time.Time
	}{{p.From, &from}, {p.To, &to}} {
		t, err := time.ParseInLocation("2006-01-02", d.s, loc)
		if err != nil || !dayPattern.MatchString(d.s) {
			return &InvalidError{fmt.Sprintf("%q is not a date like 2026-11-14", d.s)}
		}
		*d.dst = t
	}
	if to.Before(from) {
		return &InvalidError{"the period ends before it starts"}
	}
	if to.Sub(from) > (MaxDays-1)*24*time.Hour+time.Hour {
		return &InvalidError{fmt.Sprintf("a report covers at most %d days", MaxDays)}
	}
	return nil
}

// bounds is the SQL that turns the period into the instants it starts and ends at; $1 is the zone,
// $2 the first day and $3 the last, which is included.
const bounds = `($2::text::date)::timestamp AT TIME ZONE $1`
const boundsEnd = `(($3::text::date + 1)::timestamp AT TIME ZONE $1)`

// DayRow is one day of sales.
type DayRow struct {
	// PrepSeconds is the average time from payment to the order being ready, for the orders of the day
	// that are ready (0 if none).
	PrepSeconds int64  `json:"prep_seconds"`
	Date        string `json:"date"`
	Orders      int64  `json:"orders"`
	Subtotal    int64  `json:"subtotal"`
	Discount    int64  `json:"discount"`
	// Total is what was collected: the subtotal less the discount.
	Total    int64 `json:"total"`
	Cash     int64 `json:"cash"`
	Card     int64 `json:"card"`
	Transfer int64 `json:"transfer"`
	Refunds  int64 `json:"refunds"`
	// Net is what was collected less what was given back that day.
	Net int64 `json:"net"`
}

// SalesByDay returns, for each day of the period (also a day with no sales), the orders that were
// paid that day and what was given back that day.
func (s *Store) SalesByDay(ctx context.Context, p Period) ([]DayRow, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
WITH days AS (SELECT generate_series($2::text::date, $3::text::date, interval '1 day')::date AS d),
paid AS (
  SELECT (paid_at AT TIME ZONE $1)::date AS d, count(*) AS orders, sum(subtotal) AS subtotal, sum(discount) AS discount, sum(total) AS total,
         COALESCE(sum(total) FILTER (WHERE pay_method = 'cash'), 0) AS cash, COALESCE(sum(total) FILTER (WHERE pay_method = 'card'), 0) AS card,
         COALESCE(sum(total) FILTER (WHERE pay_method = 'transfer'), 0) AS transfer,
         COALESCE(avg(extract(epoch FROM ready_at - paid_at)) FILTER (WHERE ready_at IS NOT NULL), 0) AS prep
  FROM orders WHERE paid_at >= `+bounds+` AND paid_at < `+boundsEnd+` GROUP BY 1),
given AS (
  SELECT (at AT TIME ZONE $1)::date AS d, sum(amount) AS refunds FROM refunds WHERE at >= `+bounds+` AND at < `+boundsEnd+` GROUP BY 1)
SELECT days.d::text, COALESCE(paid.orders, 0)::bigint, COALESCE(paid.subtotal, 0)::bigint, COALESCE(paid.discount, 0)::bigint, COALESCE(paid.total, 0)::bigint,
       COALESCE(paid.cash, 0)::bigint, COALESCE(paid.card, 0)::bigint, COALESCE(paid.transfer, 0)::bigint, COALESCE(given.refunds, 0)::bigint,
       COALESCE(paid.prep, 0)::bigint
FROM days LEFT JOIN paid USING (d) LEFT JOIN given USING (d) ORDER BY days.d`, p.Zone, p.From, p.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayRow{}
	for rows.Next() {
		var r DayRow
		if err := rows.Scan(&r.Date, &r.Orders, &r.Subtotal, &r.Discount, &r.Total, &r.Cash, &r.Card, &r.Transfer, &r.Refunds, &r.PrepSeconds); err != nil {
			return nil, err
		}
		r.Net = r.Total - r.Refunds
		out = append(out, r)
	}
	return out, rows.Err()
}

// ItemRow is what sold of one item or combo.
type ItemRow struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Quantity int64  `json:"quantity"`
	Revenue  int64  `json:"revenue"`
	Orders   int64  `json:"orders"`
}

// BestSellers returns the items that sold most in the period, by quantity, from orders that were paid
// in it and not cancelled. The name is the latest one that was sold under.
func (s *Store) BestSellers(ctx context.Context, p Period, limit int) ([]ItemRow, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT l.kind, l.ref, (array_agg(l.name ORDER BY o.paid_at DESC))[1], (array_agg(l.category ORDER BY o.paid_at DESC))[1],
       sum(l.quantity)::bigint, sum(l.total)::bigint, count(DISTINCT o.order_id)::bigint
FROM order_lines l JOIN orders o USING (order_id)
WHERE o.paid_at >= `+bounds+` AND o.paid_at < `+boundsEnd+` AND o.status <> 'cancelled'
GROUP BY l.kind, l.ref ORDER BY 5 DESC, 6 DESC, l.ref LIMIT $4`, p.Zone, p.From, p.To, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ItemRow{}
	for rows.Next() {
		var r ItemRow
		if err := rows.Scan(&r.Kind, &r.Ref, &r.Name, &r.Category, &r.Quantity, &r.Revenue, &r.Orders); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IngredientRow is what happened to one ingredient in the period.
type IngredientRow struct {
	Ingredient string `json:"ingredient"`
	// Used is what orders took less what cancellations gave back.
	Used int64 `json:"used"`
	// Received is what arrived and Cost what was paid for it (0 where no price was written down).
	Received int64 `json:"received"`
	Cost     int64 `json:"cost"`
	// WrittenOff is what was lost; Adjusted is the signed sum of what stocktakes found.
	WrittenOff int64 `json:"written_off"`
	Adjusted   int64 `json:"adjusted"`
}

// Ingredients returns what the kitchen used, received, wasted and found different, for each
// ingredient that has any of it in the period.
func (s *Store) Ingredients(ctx context.Context, p Period) ([]IngredientRow, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
WITH used AS (SELECT ingredient, sum(qty) AS qty FROM ingredient_usage WHERE at >= `+bounds+` AND at < `+boundsEnd+` GROUP BY 1),
moved AS (
  SELECT ingredient,
         COALESCE(sum(qty) FILTER (WHERE kind = 'receive'), 0) AS received, COALESCE(sum(total_cost) FILTER (WHERE kind = 'receive'), 0) AS cost,
         COALESCE(sum(qty) FILTER (WHERE kind = 'write_off'), 0) AS wasted, COALESCE(sum(qty) FILTER (WHERE kind = 'adjust'), 0) AS adjusted
  FROM stock_movements WHERE at >= `+bounds+` AND at < `+boundsEnd+` GROUP BY 1)
SELECT COALESCE(u.ingredient, m.ingredient), COALESCE(u.qty, 0)::bigint, COALESCE(m.received, 0)::bigint, COALESCE(m.cost, 0)::bigint,
       COALESCE(m.wasted, 0)::bigint, COALESCE(m.adjusted, 0)::bigint
FROM used u FULL JOIN moved m ON m.ingredient = u.ingredient ORDER BY 1`, p.Zone, p.From, p.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IngredientRow{}
	for rows.Next() {
		var r IngredientRow
		if err := rows.Scan(&r.Ingredient, &r.Used, &r.Received, &r.Cost, &r.WrittenOff, &r.Adjusted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RefundRow is one amount given back.
type RefundRow struct {
	At     time.Time `json:"at"`
	Order  int       `json:"order"`
	Kind   string    `json:"kind"`
	Amount int64     `json:"amount"`
	Method string    `json:"method"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
}

// Refunds lists what was given back in the period, newest first, at most 1000.
func (s *Store) Refunds(ctx context.Context, p Period) ([]RefundRow, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT r.at, o.number, r.kind, r.amount, r.method, r.by, r.reason
FROM refunds r JOIN orders o USING (order_id) WHERE r.at >= `+bounds+` AND r.at < `+boundsEnd+` ORDER BY r.at DESC, r.id DESC LIMIT 1000`, p.Zone, p.From, p.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RefundRow{}
	for rows.Next() {
		var r RefundRow
		if err := rows.Scan(&r.At, &r.Order, &r.Kind, &r.Amount, &r.Method, &r.By, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HourRow is how many orders were paid in an hour of the day, over the whole period.
type HourRow struct {
	Hour   int   `json:"hour"`
	Orders int64 `json:"orders"`
	Total  int64 `json:"total"`
}

// Hours returns, for each hour of the day, the orders paid in it over the period (hours with none are
// listed with zeros), in the time zone of the venue.
func (s *Store) Hours(ctx context.Context, p Period) ([]HourRow, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
WITH h AS (SELECT extract(hour FROM paid_at AT TIME ZONE $1)::int AS hour, count(*) AS orders, sum(total) AS total
           FROM orders WHERE paid_at >= `+bounds+` AND paid_at < `+boundsEnd+` GROUP BY 1)
SELECT g, COALESCE(h.orders, 0)::bigint, COALESCE(h.total, 0)::bigint FROM generate_series(0, 23) g LEFT JOIN h ON h.hour = g ORDER BY g`, p.Zone, p.From, p.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HourRow{}
	for rows.Next() {
		var r HourRow
		if err := rows.Scan(&r.Hour, &r.Orders, &r.Total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ValidZone says whether PostgreSQL knows a time zone by that name, which is what a day of a report
// is read in.
func (s *Store) ValidZone(ctx context.Context, zone string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_timezone_names WHERE name = $1`, zone).Scan(&n)
	return n > 0, err
}

// CostDay is what the kitchen's goods cost on one day, in thousandths of a đồng.
type CostDay struct {
	Date string `json:"date"`
	// CogsMilli is what the orders paid that day took from the stock, at the price of the lots it came from,
	// less what cancellations gave back. WasteMilli is what was written off or found missing that day.
	CogsMilli  int64 `json:"cogs_milli"`
	WasteMilli int64 `json:"waste_milli"`
}

// CostsByDay returns, for each day of the period (also a day with none), the cost of the goods that
// orders used and of the goods that were wasted. An ingredient that came without a price counts for
// nothing, so a cost is what is known, and no more.
func (s *Store) CostsByDay(ctx context.Context, p Period) ([]CostDay, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
WITH days AS (SELECT generate_series($2::text::date, $3::text::date, interval '1 day')::date AS d),
used AS (SELECT (at AT TIME ZONE $1)::date AS d, sum(value_milli) AS v FROM ingredient_usage WHERE at >= `+bounds+` AND at < `+boundsEnd+` GROUP BY 1),
lost AS (SELECT (at AT TIME ZONE $1)::date AS d, sum(value_milli) AS v FROM stock_movements
         WHERE kind IN ('write_off', 'adjust', 'count') AND delta < 0 AND at >= `+bounds+` AND at < `+boundsEnd+` GROUP BY 1)
SELECT days.d::text, COALESCE(used.v, 0)::bigint, COALESCE(lost.v, 0)::bigint
FROM days LEFT JOIN used USING (d) LEFT JOIN lost USING (d) ORDER BY days.d`, p.Zone, p.From, p.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostDay{}
	for rows.Next() {
		var r CostDay
		if err := rows.Scan(&r.Date, &r.CogsMilli, &r.WasteMilli); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestUnitCosts returns, for each ingredient that came with a price, what one unit cost the last time
// it was bought, in thousandths of a đồng.
func (s *Store) LatestUnitCosts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT ON (ingredient) ingredient, (total_cost * 1000 + qty / 2) / qty
FROM stock_movements WHERE kind = 'receive' AND total_cost > 0 AND qty > 0 ORDER BY ingredient, at DESC, move_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var ing string
		var c int64
		if err := rows.Scan(&ing, &c); err != nil {
			return nil, err
		}
		out[ing] = c
	}
	return out, rows.Err()
}
