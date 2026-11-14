package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/catalog"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/inventory"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/orderservice"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
)

// openCatalog brings the schema of the catalog up to date (unless told not to) and returns its store.
func openCatalog(ctx context.Context, pool *pgxpool.Pool, cfg config, logger *slog.Logger) (*catalog.Store, error) {
	if !cfg.noMigrate {
		applied, err := pgmigrate.Apply(ctx, pool, catalog.Migrations(), pgmigrate.WithTable(catalog.MigrationsTable))
		if err != nil {
			return nil, err
		}
		for _, name := range applied {
			logger.Info("database migration applied", "migration", name)
		}
	}
	return catalog.New(pool, nil), nil
}

// startCatalog returns the menu, the stock thresholds and the promotions to start with.
//
// Without a store (the token mode) they come from the files, as they always did. With one, they
// come from the database, where the screens of the managers change them; the files are then only
// the first fill, used when the database has no version of a document yet. A document of the
// database that does not pass the checks stops the start with its number, because starting on a
// menu that nobody accepted would be worse than not starting.
func startCatalog(ctx context.Context, cfg config, store *catalog.Store, logger *slog.Logger) (*menu.Menu, map[stock.Ingredient]int64, *promo.Set, error) {
	if store == nil {
		m, low, err := orderservice.LoadMenu(cfg.menuFile)
		if err != nil {
			return nil, nil, nil, err
		}
		promos, err := orderservice.LoadPromotions(cfg.promoFile)
		if err != nil {
			return nil, nil, nil, err
		}
		return m, low, promos, nil
	}

	menuDoc, err := documentOf(ctx, store, catalog.Menu, cfg.menuFile, nil, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	m, low, err := orderservice.ParseMenu(menuDoc.Doc)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("version %d of the menu in the database does not pass the checks: %w", menuDoc.Number, err)
	}
	promoDoc, err := documentOf(ctx, store, catalog.Promotions, cfg.promoFile, []byte("[]"), logger)
	if err != nil {
		return nil, nil, nil, err
	}
	promos, err := orderservice.ParsePromotions(promoDoc.Doc)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("version %d of the promotions in the database does not pass the checks: %w", promoDoc.Number, err)
	}
	return m, low, promos, nil
}

// documentOf returns the version of a document in use. If the database has none, the file fills
// it: its text is checked first, and saved as version 1, by "system". without is what to save when
// there is no file either (no promotions); a menu with no file is an error.
func documentOf(ctx context.Context, store *catalog.Store, kind, file string, without []byte, logger *slog.Logger) (catalog.Version, error) {
	v, ok, err := store.Latest(ctx, kind)
	if err != nil {
		return catalog.Version{}, err
	}
	if ok {
		if file != "" {
			logger.Info("the database has a version of the "+kind+"; the file is not read", "file", file, "version", v.Number)
		}
		return v, nil
	}
	doc := without
	note := "the first version, empty"
	if file != "" {
		if doc, err = os.ReadFile(file); err != nil {
			return catalog.Version{}, fmt.Errorf("the database has no %s yet and its file cannot be read: %w", kind, err)
		}
		note = "filled from " + filepath.Base(file)
	}
	if doc == nil {
		return catalog.Version{}, fmt.Errorf("the database has no %s yet: give -menu with the file to fill it from", kind)
	}
	// What is saved has been through the checks that a file always went through.
	if kind == catalog.Menu {
		if _, _, err := orderservice.ParseMenu(doc); err != nil {
			return catalog.Version{}, err
		}
	} else if _, err := orderservice.ParsePromotions(doc); err != nil {
		return catalog.Version{}, err
	}
	saved, err := store.Save(ctx, kind, 0, doc, "system", note)
	if err != nil {
		return catalog.Version{}, err
	}
	logger.Info("the database had no "+kind+": version 1 was filled in", "from", note)
	return saved, nil
}

// openInventory brings the schema of the warehouse data up to date (unless told not to) and returns
// its store.
func openInventory(ctx context.Context, pool *pgxpool.Pool, cfg config, logger *slog.Logger) (*inventory.Store, error) {
	if !cfg.noMigrate {
		applied, err := pgmigrate.Apply(ctx, pool, inventory.Migrations(), pgmigrate.WithTable(inventory.MigrationsTable))
		if err != nil {
			return nil, err
		}
		for _, name := range applied {
			logger.Info("database migration applied", "migration", name)
		}
	}
	return inventory.New(pool, nil), nil
}
