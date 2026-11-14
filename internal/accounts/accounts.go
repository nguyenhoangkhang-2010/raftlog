// Package accounts is who may use the kitchen system: people with a login name, a password,
// roles, and sessions, kept in PostgreSQL.
//
// What it decides, and why it is stricter than the token file it replaces:
//
//   - A password is kept only as an argon2id hash. A wrong password, an unknown login name and
//     a disabled account all answer alike and cost alike, so the answer does not say which
//     login names exist.
//   - Five wrong passwords in a row lock the account for a quarter of an hour. This protects
//     the password from guessing and gives a stranger a way to lock someone out; the
//     administrator can unlock, and the per-address limit at the HTTP layer slows the stranger.
//   - A session is a random token whose hash is stored. It expires when idle and at an absolute
//     age, and is revoked at once when the account is disabled, its password is reset, or the
//     person asks.
//   - Roles are read from the database at every request, never copied into the session, so
//     taking a role away or disabling an account takes effect on the next request.
//   - The last administrator can neither be disabled nor lose the role, and nobody changes
//     their own roles or disables themselves.
//   - Everything that matters is written to account_events, in the same transaction as the
//     change, and the table cannot be updated or deleted from.
//
// The accounts are separate from the orders: an order carries the login name of the people who
// touched it, and that name never changes.
package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the SQL files of this package, for the migrator.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

// Config holds the settings of a Service. A zero field takes its default.
type Config struct {
	IdleTimeout time.Duration // a session unused this long ends; default 12 hours
	MaxLifetime time.Duration // a session ends this long after sign-in at the latest; default 7 days
	MaxFailures int           // wrong passwords in a row before the lock; default 5
	LockFor     time.Duration // how long the lock lasts; default 15 minutes
	Hash        HashParams    // default DefaultHash
	MaxHashes   int           // password hashes computed at once; default 4
	Now         func() time.Time
}

func (c *Config) fill() {
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 12 * time.Hour
	}
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = 7 * 24 * time.Hour
	}
	if c.MaxFailures <= 0 {
		c.MaxFailures = 5
	}
	if c.LockFor <= 0 {
		c.LockFor = 15 * time.Minute
	}
	if c.Hash == (HashParams{}) {
		c.Hash = DefaultHash
	}
	if c.MaxHashes <= 0 {
		c.MaxHashes = 4
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Client says where a request came from, for the trail and the list of sessions.
type Client struct {
	IP        string
	UserAgent string
}

// A User is an account, without its password.
type User struct {
	ID                 string
	DisplayName        string
	Roles              []rbac.Role
	Station            string
	Disabled           bool
	MustChangePassword bool
	FailedAttempts     int
	LockedUntil        *time.Time
	CreatedAt          time.Time
	CreatedBy          string
	LastLoginAt        *time.Time
	PasswordChangedAt  time.Time
	// ActiveSessions is filled by ListUsers only.
	ActiveSessions int
}

// Actor is the user as the order rules see them.
func (u User) Actor() rbac.Actor {
	return rbac.Actor{ID: u.ID, Roles: append([]rbac.Role(nil), u.Roles...), Station: u.Station}
}

// A Session is one signed-in browser.
type Session struct {
	ID                int64
	User              User
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	IP                string
	UserAgent         string
}

// ExpiresAt is when the session ends if it is not used again.
func (s Session) ExpiresAt() time.Time {
	if s.AbsoluteExpiresAt.Before(s.IdleExpiresAt) {
		return s.AbsoluteExpiresAt
	}
	return s.IdleExpiresAt
}

// An Event is a line of the trail.
type Event struct {
	ID      int64
	At      time.Time
	Actor   string
	Action  string
	Subject string
	Detail  string // JSON
	IP      string
}

// NewUser is what is needed to create an account.
type NewUser struct {
	ID          string
	DisplayName string
	Roles       []rbac.Role
	Station     string
}

// UserUpdate replaces the editable fields of an account.
type UserUpdate struct {
	DisplayName string
	Roles       []rbac.Role
	Station     string
}

// --- errors ------------------------------------------------------------------------------

var (
	// ErrBadCredentials answers a wrong password, an unknown login name and a disabled account.
	ErrBadCredentials = errors.New("accounts: wrong login name or password")
	// ErrNoSession says that a token is not that of a live session.
	ErrNoSession = errors.New("accounts: no such session")
	// ErrNotFound says that there is no such account or session.
	ErrNotFound = errors.New("accounts: not found")
	// ErrExists says that the login name is taken.
	ErrExists = errors.New("accounts: that login name is taken")
	// ErrSelf says that an administrator tried to do to their own account what only another may.
	ErrSelf = errors.New("accounts: you cannot do that to your own account")
	// ErrLastAdmin says that the change would leave nobody able to manage accounts.
	ErrLastAdmin = errors.New("accounts: that would leave no active administrator")
)

// LockedError says that the account is locked, and until when.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return "accounts: too many wrong passwords; the account is locked until " + e.Until.Format(time.RFC3339)
}

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "accounts: " + e.Reason }

func invalid(format string, a ...any) error { return &InvalidError{Reason: fmt.Sprintf(format, a...)} }

// --- validation --------------------------------------------------------------------------

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)
	stationPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,19}$`)
)

func normalizeID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

func validateNewUser(in NewUser) (NewUser, error) {
	in.ID = normalizeID(in.ID)
	if !idPattern.MatchString(in.ID) {
		return in, invalid("a login name is 3 to 32 characters: lower-case letters, digits, dots, dashes and underscores, starting with a letter or digit")
	}
	var err error
	if in.DisplayName, in.Roles, in.Station, err = validateEditable(in.DisplayName, in.Roles, in.Station); err != nil {
		return in, err
	}
	return in, nil
}

func validateEditable(name string, roles []rbac.Role, station string) (string, []rbac.Role, string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 60 {
		return "", nil, "", invalid("the name is 1 to 60 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", nil, "", invalid("the name has a control character")
		}
	}
	if len(roles) == 0 {
		return "", nil, "", invalid("give at least one role")
	}
	seen := map[rbac.Role]bool{}
	var clean []rbac.Role
	for _, r := range roles {
		if !r.Valid() {
			return "", nil, "", invalid("%q is not a role", string(r))
		}
		if !seen[r] {
			seen[r] = true
			clean = append(clean, r)
		}
	}
	station = strings.ToLower(strings.TrimSpace(station))
	switch {
	case seen[rbac.Kitchen] && station == "":
		return "", nil, "", invalid("a kitchen worker works at a station: give one")
	case !seen[rbac.Kitchen] && station != "":
		return "", nil, "", invalid("only a kitchen worker has a station")
	case station != "" && !stationPattern.MatchString(station):
		return "", nil, "", invalid("a station is 2 to 20 lower-case letters, digits or dashes")
	}
	return name, clean, station, nil
}

func rolesToStrings(roles []rbac.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

func stringsToRoles(in []string) []rbac.Role {
	out := make([]rbac.Role, len(in))
	for i, r := range in {
		out[i] = rbac.Role(r)
	}
	return out
}

// --- tokens ------------------------------------------------------------------------------

// tokenLength is the length of a session token as text: 32 random bytes, base64 without padding.
const tokenLength = 43

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("accounts: no randomness for a session: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// hashToken is what is stored of a token. The token is 256 random bits, so a plain SHA-256 is
// enough: there is nothing to guess that a slow hash would make harder.
func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
