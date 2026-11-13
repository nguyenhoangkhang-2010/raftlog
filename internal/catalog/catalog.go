// Package catalog keeps the menu and the promotions of the kitchen system in PostgreSQL, as
// versions, and the photos of the items.
//
// A document (the menu, the promotions) is stored whole, as the JSON that the files of the same
// name hold. This package does not read it: whoever saves a document has checked it with the code
// that reads it, so a version in the database is one that the service has already accepted. A
// save names the version that the editor started from, and is refused if somebody saved in the
// meantime, so that two people editing do not overwrite each other without knowing.
package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // the formats of a photo
	_ "image/png"
	"io/fs"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationsTable is where the migrator records what it applied: the numbers of these files
// start from 1, like those of the accounts, so the two keep their own tables.
const MigrationsTable = "catalog_migrations"

// Migrations returns the SQL files of this package, for the migrator.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

// The kinds of document.
const (
	Menu       = "menu"
	Promotions = "promotions"
)

// A Version is one saved state of a document.
type Version struct {
	Kind   string
	Number int
	Doc    json.RawMessage // empty in a list
	At     time.Time
	By     string
	Note   string
}

// Errors of the store.
var (
	// ErrStale says that the document was saved by someone else after the one the editor started from.
	ErrStale = errors.New("catalog: the document was changed by someone else; read it again")
	// ErrNoVersion says that there is no such version.
	ErrNoVersion = errors.New("catalog: no such version")
	// ErrNoImage says that the item has no photo.
	ErrNoImage = errors.New("catalog: no photo")
)

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "catalog: " + e.Reason }

// Store is the catalog of a database.
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

func checkKind(kind string) error {
	if kind != Menu && kind != Promotions {
		return &InvalidError{fmt.Sprintf("%q is not a kind of document", kind)}
	}
	return nil
}

// Latest returns the version in use, and false if the document has never been saved.
func (s *Store) Latest(ctx context.Context, kind string) (Version, bool, error) {
	if err := checkKind(kind); err != nil {
		return Version{}, false, err
	}
	var v Version
	err := s.pool.QueryRow(ctx, `SELECT kind, version, doc, created_at, created_by, note FROM catalog_versions WHERE kind = $1 ORDER BY version DESC LIMIT 1`, kind).
		Scan(&v.Kind, &v.Number, &v.Doc, &v.At, &v.By, &v.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, false, nil
	}
	return v, err == nil, err
}

// Get returns one version.
func (s *Store) Get(ctx context.Context, kind string, number int) (Version, error) {
	if err := checkKind(kind); err != nil {
		return Version{}, err
	}
	var v Version
	err := s.pool.QueryRow(ctx, `SELECT kind, version, doc, created_at, created_by, note FROM catalog_versions WHERE kind = $1 AND version = $2`, kind, number).
		Scan(&v.Kind, &v.Number, &v.Doc, &v.At, &v.By, &v.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNoVersion
	}
	return v, err
}

// List returns the versions of a document, newest first, without the documents themselves.
func (s *Store) List(ctx context.Context, kind string, limit int) ([]Version, error) {
	if err := checkKind(kind); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, version, created_at, created_by, note FROM catalog_versions WHERE kind = $1 ORDER BY version DESC LIMIT $2`, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.Kind, &v.Number, &v.At, &v.By, &v.Note); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Save stores a document as the next version. base is the number of the version the editor
// started from (0 when nothing was saved yet); if another version has been saved since, it is
// ErrStale and nothing is written. The document must be valid JSON; whether it is a valid menu is
// for the caller to have checked.
func (s *Store) Save(ctx context.Context, kind string, base int, doc []byte, by, note string) (Version, error) {
	if err := checkKind(kind); err != nil {
		return Version{}, err
	}
	if !json.Valid(doc) {
		return Version{}, &InvalidError{"the document is not JSON"}
	}
	if by == "" {
		return Version{}, &InvalidError{"say who saves"}
	}
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > 200 {
		return Version{}, &InvalidError{"a note is at most 200 characters"}
	}
	v := Version{Kind: kind, Number: base + 1, Doc: doc, At: s.now(), By: by, Note: note}
	// The primary key (kind, version) is the guard: two saves that started from the same version
	// both want the next number, and only one gets it.
	// A save that started from an older version writes nothing, because its number is not the next
	// one (the SELECT is empty).
	tag, err := s.pool.Exec(ctx, `INSERT INTO catalog_versions (kind, version, doc, created_at, created_by, note)
		SELECT $1, $2, $3::jsonb, $4, $5, $6
		WHERE $2 = 1 + COALESCE((SELECT max(version) FROM catalog_versions WHERE kind = $1), 0)`,
		kind, v.Number, string(doc), v.At, by, note)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return Version{}, ErrStale
		}
		return Version{}, err
	}
	if tag.RowsAffected() == 0 {
		return Version{}, ErrStale
	}
	return v, nil
}

// --- photos --------------------------------------------------------------------------------

// Photo limits: a photo is a JPEG or a PNG of at most a megabyte and 4096 pixels on a side, and
// at least 64, so that a file that is not a photo (or is a decompression bomb) is refused before
// it is stored or shown.
const (
	MaxPhotoBytes = 1 << 20
	maxSide       = 4096
	minSide       = 64
)

// An Image is the photo of an item.
type Image struct {
	ItemID      string
	ContentType string
	Data        []byte
	Hash        string
	At          time.Time
}

// CheckImage returns the content type of a photo, or why it is refused. The type is read from the
// bytes, never from what the client said.
func CheckImage(data []byte) (string, error) {
	if len(data) == 0 {
		return "", &InvalidError{"the photo is empty"}
	}
	if len(data) > MaxPhotoBytes {
		return "", &InvalidError{fmt.Sprintf("a photo is at most %d KB", MaxPhotoBytes/1024)}
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", &InvalidError{"the file is not a JPEG or a PNG photo"}
	}
	if cfg.Width < minSide || cfg.Height < minSide || cfg.Width > maxSide || cfg.Height > maxSide {
		return "", &InvalidError{fmt.Sprintf("a photo is between %d and %d pixels on each side", minSide, maxSide)}
	}
	switch format {
	case "jpeg":
		return "image/jpeg", nil
	case "png":
		return "image/png", nil
	}
	return "", &InvalidError{"the file is not a JPEG or a PNG photo"}
}

// PutImage stores the photo of an item, replacing the one it had, and returns its hash.
func (s *Store) PutImage(ctx context.Context, itemID string, data []byte, by string) (string, error) {
	ct, err := CheckImage(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	_, err = s.pool.Exec(ctx, `INSERT INTO item_images (item_id, content_type, data, sha256, updated_at, updated_by) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (item_id) DO UPDATE SET content_type = EXCLUDED.content_type, data = EXCLUDED.data, sha256 = EXCLUDED.sha256, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		itemID, ct, data, hash, s.now(), by)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23514" { // check_violation: the item id is not one
			return "", &InvalidError{"that is not an item id"}
		}
		return "", err
	}
	return hash, nil
}

// Image returns the photo of an item.
func (s *Store) Image(ctx context.Context, itemID string) (Image, error) {
	var im Image
	err := s.pool.QueryRow(ctx, `SELECT item_id, content_type, data, sha256, updated_at FROM item_images WHERE item_id = $1`, itemID).
		Scan(&im.ItemID, &im.ContentType, &im.Data, &im.Hash, &im.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return Image{}, ErrNoImage
	}
	return im, err
}

// DeleteImage removes the photo of an item; an item with no photo is not an error.
func (s *Store) DeleteImage(ctx context.Context, itemID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM item_images WHERE item_id = $1`, itemID)
	return err
}

// ImageHashes returns the hash of the photo of each item that has one, which the menu uses to
// build the address of a photo so that a browser fetches it again only when it changed.
func (s *Store) ImageHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT item_id, sha256 FROM item_images`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}
