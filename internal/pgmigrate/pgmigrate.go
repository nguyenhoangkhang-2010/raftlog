// Package pgmigrate applies numbered SQL files to a PostgreSQL database: each file once, in
// order, each in its own transaction.
//
// A file is named NNNN_what_it_does.sql, where NNNN is its version. What was applied is recorded
// in the table schema_migrations together with a checksum of the file, so that a file edited
// after it was applied is reported instead of silently diverging from the database. Running the
// migrator on several service instances at once is safe: they take turns on an advisory lock.
//
// There is no "down" direction. A migration that went wrong is fixed by a new migration; a
// database is restored from a backup, not by a script that was never run on real data.
package pgmigrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey is the advisory lock that serialises migrators (the bytes of "raftmigr").
const lockKey int64 = 0x72616674_6d696772

var fileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// A Migration is one file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Load reads the migrations in fsys (the files of its top directory), in version order. A file
// that does not match the naming, a repeated version or a gap is an error: they are the signs of
// a merge that went wrong, and finding out at start is cheaper than on a customer's database.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("pgmigrate: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("pgmigrate: %q is not named NNNN_name.sql", e.Name())
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("pgmigrate: %w", err)
		}
		v, _ := strconv.Atoi(m[1])
		// The checksum is of the text, not of how a checkout wrote its line ends: the same file is
		// LF on one machine and CRLF on another (git's autocrlf), and must be the same migration.
		sum := sha256.Sum256(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
		out = append(out, Migration{Version: v, Name: m[1] + "_" + m[2], SQL: string(data), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("pgmigrate: after version %d comes %d, so a file is missing or repeated", i, m.Version)
		}
	}
	return out, nil
}

// An Option changes how Apply records what it has done.
type Option func(*settings)

type settings struct{ table string }

// WithTable keeps the record of what was applied in another table than schema_migrations. Each
// set of migrations (the accounts, the catalog) numbers its files from 0001 and keeps its own
// table, so that the sets do not have to know about each other.
func WithTable(name string) Option { return func(s *settings) { s.table = name } }

var tableName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func tableOf(options []Option) (string, error) {
	s := settings{table: "schema_migrations"}
	for _, o := range options {
		o(&s)
	}
	if !tableName.MatchString(s.table) {
		return "", fmt.Errorf("pgmigrate: %q is not a table name", s.table)
	}
	return s.table, nil
}

// ErrChanged says that a migration that was applied has been edited since.
var ErrChanged = errors.New("pgmigrate: an applied migration was changed")

// Apply brings the database up to date and returns the names of the migrations it applied.
func Apply(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, options ...Option) ([]string, error) {
	table, err := tableOf(options)
	if err != nil {
		return nil, err
	}
	all, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgmigrate: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("pgmigrate: lock: %w", err)
	}
	// The lock is tied to the connection, which goes back to the pool: release it explicitly,
	// and with a context that survives a cancelled one.
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		version int PRIMARY KEY, name text NOT NULL, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("pgmigrate: %w", err)
	}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM `+table)
	if err != nil {
		return nil, fmt.Errorf("pgmigrate: %w", err)
	}
	applied := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pgmigrate: %w", err)
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgmigrate: %w", err)
	}
	for v := range applied {
		if v > len(all) {
			return nil, fmt.Errorf("pgmigrate: the database has version %d, newer than this program knows (%d): run a newer program", v, len(all))
		}
	}

	var done []string
	for _, m := range all {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return done, fmt.Errorf("%w: %s", ErrChanged, m.Name)
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return done, fmt.Errorf("pgmigrate: %w", err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return done, fmt.Errorf("pgmigrate: %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (version, name, checksum) VALUES ($1, $2, $3)`, m.Version, m.Name, m.Checksum); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return done, fmt.Errorf("pgmigrate: %s: %w", m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return done, fmt.Errorf("pgmigrate: %s: %w", m.Name, err)
		}
		done = append(done, m.Name)
	}
	return done, nil
}
