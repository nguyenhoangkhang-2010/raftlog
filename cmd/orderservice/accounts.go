package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
)

// openAccounts connects to the accounts database, brings its schema up to date (unless told not
// to) and returns the accounts service. The URL holds a password, so it is never put in an error
// or a log line.
func openAccounts(ctx context.Context, cfg config, logger *slog.Logger) (*pgxpool.Pool, *accounts.Service, error) {
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("the database address is not valid: %s", scrub(err.Error(), cfg.databaseURL))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("the accounts database does not answer: %s", scrub(err.Error(), cfg.databaseURL))
	}
	if !cfg.noMigrate {
		applied, err := pgmigrate.Apply(ctx, pool, accounts.Migrations())
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		for _, name := range applied {
			logger.Info("database migration applied", "migration", name)
		}
	}
	acc, err := accounts.New(pool, accounts.Config{IdleTimeout: cfg.sessionIdle, MaxLifetime: cfg.sessionMax})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return pool, acc, nil
}

// scrub removes the database URL (and so its password) from the text of an error.
func scrub(msg, url string) string {
	if url == "" {
		return msg
	}
	return strings.ReplaceAll(msg, url, "<database-url>")
}

// purgeSessions deletes, once an hour, the sessions that ended long ago.
func purgeSessions(ctx context.Context, acc *accounts.Service, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, time.Minute)
			if n, err := acc.Purge(pctx); err != nil {
				logger.Warn("could not delete old sessions", "err", err)
			} else if n > 0 {
				logger.Info("old sessions deleted", "count", n)
			}
			cancel()
		}
	}
}
