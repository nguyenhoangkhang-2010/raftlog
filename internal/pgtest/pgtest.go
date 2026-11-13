// Package pgtest gives a test its own corner of a real PostgreSQL database.
//
// The database is the one named by RAFTLOG_TEST_DATABASE_URL, which must be a throwaway: the
// tests create and drop schemas in it. When the variable is not set the test is skipped, with
// the reason, so the suite still passes on a machine without PostgreSQL. In CI the variable is
// set, so the same tests run for real there.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL is the variable that holds the connection string of the test database.
const EnvURL = "RAFTLOG_TEST_DATABASE_URL"

// Pool returns a pool whose connections use a new, empty schema, dropped when the test ends. Two
// tests, even in parallel, never see each other's tables.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := URL(t)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// URL returns the address of the test database with a new, empty schema in its search path, for
// code that opens its own connections (a command that is given a database address). The schema
// is dropped when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("%s is not set: the tests that need PostgreSQL are skipped", EnvURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(b[:])

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatalf("create a schema in the test database: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dcancel()
		if _, err := admin.Exec(dctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop the test schema %s: %v", schema, err)
		}
		admin.Close()
	})
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema
}
