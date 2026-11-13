package pgmigrate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

func files(sql ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i, s := range sql {
		fsys[name(i+1)] = &fstest.MapFile{Data: []byte(s)}
	}
	return fsys
}

func name(v int) string {
	return string([]byte{'0', '0', byte('0' + v/10), byte('0' + v%10)}) + "_step.sql"
}

func TestLoadRefusesWhatLooksLikeAMergeThatWentWrong(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"a name that does not fit": {"init.sql": {Data: []byte("select 1")}},
		"a gap":                    {"0001_a.sql": {Data: []byte("select 1")}, "0003_c.sql": {Data: []byte("select 1")}},
		"a repeated version":       {"0001_a.sql": {Data: []byte("select 1")}, "0001_b.sql": {Data: []byte("select 1")}},
		"not starting at one":      {"0002_b.sql": {Data: []byte("select 1")}},
	}
	for what, fsys := range cases {
		if _, err := Load(fsys); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	got, err := Load(files("select 1", "select 2"))
	if err != nil || len(got) != 2 || got[0].Version != 1 || got[1].Version != 2 || got[0].Checksum == got[1].Checksum {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestApplyRunsEachMigrationOnceAndInOrder(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	fsys := files(`CREATE TABLE a (n int)`, `INSERT INTO a VALUES (1); CREATE TABLE b (n int)`)
	done, err := Apply(ctx, pool, fsys)
	if err != nil || len(done) != 2 {
		t.Fatalf("first Apply: %v, %v", done, err)
	}
	done, err = Apply(ctx, pool, fsys)
	if err != nil || len(done) != 0 {
		t.Fatalf("second Apply: %v, %v", done, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM a`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows in a = %d, %v: the migration ran twice or not at all", n, err)
	}
	// A new file is applied on top of the old ones.
	fsys[name(3)] = &fstest.MapFile{Data: []byte(`CREATE TABLE c (n int)`)}
	if done, err = Apply(ctx, pool, fsys); err != nil || len(done) != 1 {
		t.Fatalf("third Apply: %v, %v", done, err)
	}
}

func TestAMigrationThatFailsLeavesNothingOfItself(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	_, err := Apply(ctx, pool, files(`CREATE TABLE a (n int)`, `CREATE TABLE half (n int); SELECT 1/0`))
	if err == nil {
		t.Fatal("a failing migration was reported as a success")
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('half') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("half of a failed migration was left behind (%v, %v)", exists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regclass('a') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatal("the migration before the failing one was lost")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d migrations recorded, want 1", n)
	}
}

func TestAnEditedMigrationIsReported(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	if _, err := Apply(ctx, pool, files(`CREATE TABLE a (n int)`)); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(ctx, pool, files(`CREATE TABLE a (n bigint)`))
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("an edited migration gave %v", err)
	}
}

func TestADatabaseNewerThanTheProgramIsRefused(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	if _, err := Apply(ctx, pool, files(`CREATE TABLE a (n int)`, `CREATE TABLE b (n int)`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, pool, files(`CREATE TABLE a (n int)`)); err == nil {
		t.Fatal("an older program was allowed to run on a newer database")
	}
}

func TestInstancesStartingTogetherApplyEachMigrationOnce(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	fsys := files(`CREATE TABLE a (n int)`, `INSERT INTO a VALUES (1)`)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Apply(ctx, pool, fsys)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent Apply failed: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM a`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v: a migration ran more than once", n, err)
	}
}
