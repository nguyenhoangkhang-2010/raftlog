package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// Service is the accounts of a database.
type Service struct {
	pool  *pgxpool.Pool
	cfg   Config
	sem   chan struct{} // limits the password hashes computed at once
	dummy string        // a hash to spend the same time on a login name that does not exist
}

// New returns a Service on a database whose migrations (see Migrations) have been applied.
func New(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	cfg.fill()
	dummy, err := hashPassword("a password that nobody has", cfg.Hash)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, cfg: cfg, sem: make(chan struct{}, cfg.MaxHashes), dummy: dummy}, nil
}

// Ping checks that the database answers.
func (s *Service) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Service) now() time.Time { return s.cfg.Now().UTC().Truncate(time.Microsecond) }

// --- shared pieces -----------------------------------------------------------------------

const userCols = "id, display_name, roles, station, disabled, must_change_password, failed_attempts, locked_until, created_at, created_by, last_login_at, password_changed_at"

// cols is userCols with a table alias in front of each column.
func cols(alias string) string {
	parts := strings.Split(userCols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// scanUser reads a row that starts with the columns of userCols, then those of dest.
func scanUser(row pgx.Row, dest ...any) (User, error) {
	var u User
	var roles []string
	args := append([]any{&u.ID, &u.DisplayName, &roles, &u.Station, &u.Disabled, &u.MustChangePassword, &u.FailedAttempts,
		&u.LockedUntil, &u.CreatedAt, &u.CreatedBy, &u.LastLoginAt, &u.PasswordChangedAt}, dest...)
	if err := row.Scan(args...); err != nil {
		return User{}, err
	}
	u.Roles = stringsToRoles(roles)
	return u, nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// record adds a line to the trail. Inside a transaction it commits or rolls back with the change.
func (s *Service) record(ctx context.Context, q execer, actor, action, subject string, detail map[string]any, ip string) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO account_events (at, actor, action, subject, detail, ip) VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
		s.now(), actor, action, subject, string(b), ip)
	return err
}

// hash and verify spend a limited number of password hashes at a time: each takes 64 MiB, and a
// burst of sign-ins must not be able to exhaust the memory of the machine.
func (s *Service) acquire(ctx context.Context) (release func(), err error) {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) hash(ctx context.Context, password string) (string, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return hashPassword(password, s.cfg.Hash)
}

func (s *Service) verify(ctx context.Context, password, encoded string) (ok, rehash bool, err error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return false, false, err
	}
	defer release()
	return verifyPassword(password, encoded, s.cfg.Hash)
}

// burn spends the time of a check on a login name that does not exist.
func (s *Service) burn(ctx context.Context, password string) {
	_, _, _ = s.verify(ctx, password, s.dummy)
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// failure counts a wrong password against an account (in the caller's transaction, with the
// row locked) and says whether it locked the account.
func (s *Service) failure(ctx context.Context, tx pgx.Tx, u User, action string, c Client) (*LockedError, error) {
	now := s.now()
	attempts := u.FailedAttempts + 1
	if attempts >= s.cfg.MaxFailures {
		until := now.Add(s.cfg.LockFor)
		if _, err := tx.Exec(ctx, `UPDATE users SET failed_attempts = 0, locked_until = $2, updated_at = $3 WHERE id = $1`, u.ID, until, now); err != nil {
			return nil, err
		}
		if err := s.record(ctx, tx, u.ID, "login.locked", u.ID, map[string]any{"until": until, "after": action}, c.IP); err != nil {
			return nil, err
		}
		return &LockedError{Until: until}, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET failed_attempts = $2, updated_at = $3 WHERE id = $1`, u.ID, attempts, now); err != nil {
		return nil, err
	}
	return nil, s.record(ctx, tx, "", action, u.ID, map[string]any{"attempt": attempts}, c.IP)
}

// --- signing in and out ------------------------------------------------------------------

// Login checks a login name and password and, if they are right, starts a session and returns
// its token: the only time it exists in the clear. A wrong password, an unknown name and a
// disabled account all return ErrBadCredentials after about the same time.
func (s *Service) Login(ctx context.Context, id, password string, c Client) (string, Session, error) {
	id = normalizeID(id)
	if len(password) > 1024 || !idPattern.MatchString(id) {
		s.burn(ctx, password)
		_ = s.record(ctx, s.pool, "", "login.unknown", "", map[string]any{"login": truncate(id, 40)}, c.IP)
		return "", Session{}, ErrBadCredentials
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", Session{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var hash string
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+`, password_hash FROM users WHERE id = $1 FOR UPDATE`, id), &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		s.burn(ctx, password)
		_ = s.record(ctx, s.pool, "", "login.unknown", id, nil, c.IP)
		return "", Session{}, ErrBadCredentials
	}
	if err != nil {
		return "", Session{}, err
	}
	now := s.now()
	if u.LockedUntil != nil && now.Before(*u.LockedUntil) {
		if err := s.record(ctx, tx, "", "login.blocked", u.ID, nil, c.IP); err != nil {
			return "", Session{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", Session{}, err
		}
		return "", Session{}, &LockedError{Until: *u.LockedUntil}
	}
	ok, rehash, err := s.verify(ctx, password, hash)
	if err != nil {
		return "", Session{}, err
	}
	if !ok || u.Disabled {
		action := "login.failure"
		if ok { // the right password for an account that is off: not counted, so it cannot be locked by it
			if err := s.record(ctx, tx, "", "login.disabled", u.ID, nil, c.IP); err != nil {
				return "", Session{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return "", Session{}, err
			}
			return "", Session{}, ErrBadCredentials
		}
		locked, err := s.failure(ctx, tx, u, action, c)
		if err != nil {
			return "", Session{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", Session{}, err
		}
		if locked != nil {
			return "", Session{}, locked
		}
		return "", Session{}, ErrBadCredentials
	}

	if rehash {
		if h, err := s.hash(ctx, password); err == nil {
			hash = h
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET failed_attempts = 0, locked_until = NULL, last_login_at = $2, password_hash = $3, updated_at = $2 WHERE id = $1`,
		u.ID, now, hash); err != nil {
		return "", Session{}, err
	}
	token, err := newToken()
	if err != nil {
		return "", Session{}, err
	}
	sess := Session{User: u, CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: now.Add(s.cfg.MaxLifetime), IP: c.IP, UserAgent: truncate(c.UserAgent, 200)}
	sess.IdleExpiresAt = minTime(now.Add(s.cfg.IdleTimeout), sess.AbsoluteExpiresAt)
	if err := tx.QueryRow(ctx, `INSERT INTO sessions (user_id, token_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at, ip, user_agent)
		VALUES ($1, $2, $3, $3, $4, $5, $6, $7) RETURNING id`, u.ID, hashToken(token), now, sess.IdleExpiresAt, sess.AbsoluteExpiresAt, sess.IP, sess.UserAgent).Scan(&sess.ID); err != nil {
		return "", Session{}, err
	}
	if err := s.record(ctx, tx, u.ID, "login.success", u.ID, map[string]any{"session": sess.ID}, c.IP); err != nil {
		return "", Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", Session{}, err
	}
	sess.User.FailedAttempts, sess.User.LockedUntil = 0, nil
	return token, sess, nil
}

// Authenticate returns the live session of a token and moves its idle expiry forward. The
// account is read at the same time, so its roles and its disabled flag are those of this moment.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	return s.authenticate(ctx, token, true)
}

// Peek is Authenticate for a caller that is only checking that the session is still good, such
// as a stream that has been open for hours: it does not count as use, so a forgotten screen
// does not keep its session from going idle.
func (s *Service) Peek(ctx context.Context, token string) (Session, error) {
	return s.authenticate(ctx, token, false)
}

func (s *Service) authenticate(ctx context.Context, token string, slide bool) (Session, error) {
	if len(token) != tokenLength {
		return Session{}, ErrNoSession
	}
	now := s.now()
	var sess Session
	row := s.pool.QueryRow(ctx, `SELECT `+cols("u")+`, s.id, s.created_at, s.last_seen_at, s.idle_expires_at, s.absolute_expires_at, s.ip, s.user_agent
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.idle_expires_at > $2 AND s.absolute_expires_at > $2 AND NOT u.disabled`,
		hashToken(token), now)
	u, err := scanUser(row, &sess.ID, &sess.CreatedAt, &sess.LastSeenAt, &sess.IdleExpiresAt, &sess.AbsoluteExpiresAt, &sess.IP, &sess.UserAgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	sess.User = u
	// Writing at every request would make every request a write; once a minute is enough for
	// "idle for twelve hours" to be right to the minute.
	if slide && now.Sub(sess.LastSeenAt) >= time.Minute {
		idle := minTime(now.Add(s.cfg.IdleTimeout), sess.AbsoluteExpiresAt)
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2, idle_expires_at = $3 WHERE id = $1`, sess.ID, now, idle); err == nil {
			sess.LastSeenAt, sess.IdleExpiresAt = now, idle
		}
	}
	return sess, nil
}

// Logout ends the session of a token. A token that is not a live session is not an error.
func (s *Service) Logout(ctx context.Context, token string, c Client) error {
	if len(token) != tokenLength {
		return nil
	}
	var id int64
	var user string
	err := s.pool.QueryRow(ctx, `UPDATE sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL RETURNING id, user_id`,
		hashToken(token), s.now()).Scan(&id, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.record(ctx, s.pool, user, "logout", user, map[string]any{"session": id}, c.IP)
}

// ChangePassword changes the password of someone who knows the current one. Every other session
// of the account ends; keep is the session that asked, which stays.
func (s *Service) ChangePassword(ctx context.Context, id string, keep int64, current, next string, c Client) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var hash string
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+`, password_hash FROM users WHERE id = $1 FOR UPDATE`, id), &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := s.now()
	if u.LockedUntil != nil && now.Before(*u.LockedUntil) {
		return &LockedError{Until: *u.LockedUntil}
	}
	ok, _, err := s.verify(ctx, current, hash)
	if err != nil {
		return err
	}
	if !ok {
		locked, err := s.failure(ctx, tx, u, "password.wrong_current", c)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if locked != nil {
			return locked
		}
		return ErrBadCredentials
	}
	if err := CheckPassword(next, u.ID, u.DisplayName); err != nil {
		return err
	}
	if next == current {
		return &PasswordError{"choose a password that is different from the current one"}
	}
	newHash, err := s.hash(ctx, next)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, must_change_password = false, failed_attempts = 0, password_changed_at = $3, updated_at = $3 WHERE id = $1`,
		u.ID, newHash, now); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, u.ID, keep, now)
	if err != nil {
		return err
	}
	if err := s.record(ctx, tx, u.ID, "password.change", u.ID, map[string]any{"sessions_ended": tag.RowsAffected()}, c.IP); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- managing accounts -------------------------------------------------------------------

// CreateUser creates an account with a random temporary password, which is returned once and
// must be changed at the first sign-in. by is the login name of the administrator.
func (s *Service) CreateUser(ctx context.Context, by string, in NewUser, c Client) (User, string, error) {
	temp, err := NewTemporaryPassword()
	if err != nil {
		return User{}, "", err
	}
	u, err := s.create(ctx, by, "user.create", in, temp, true, c)
	if err != nil {
		return User{}, "", err
	}
	return u, temp, nil
}

// Register makes the account of a person who signs up by themselves, with the password they chose. The account
// is always a customer's and nothing else: what the person asked for decides only the login name, the name and
// the password. The trail says that the account was made by its own owner.
func (s *Service) Register(ctx context.Context, id, displayName, password string, c Client) (User, error) {
	in, err := validateNewUser(NewUser{ID: id, DisplayName: displayName, Roles: []rbac.Role{rbac.Customer}})
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(password, in.ID, in.DisplayName); err != nil {
		return User{}, err
	}
	return s.create(ctx, in.ID, "user.register", in, password, false, c)
}

// CreateWithPassword creates an account with a password that someone chose, which must follow
// the policy. It is for the command line, which creates the first administrator.
func (s *Service) CreateWithPassword(ctx context.Context, by string, in NewUser, password string) (User, error) {
	in, err := validateNewUser(in)
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(password, in.ID, in.DisplayName); err != nil {
		return User{}, err
	}
	return s.create(ctx, by, "user.create", in, password, false, Client{})
}

func (s *Service) create(ctx context.Context, by, action string, in NewUser, password string, must bool, c Client) (User, error) {
	in, err := validateNewUser(in)
	if err != nil {
		return User{}, err
	}
	hash, err := s.hash(ctx, password)
	if err != nil {
		return User{}, err
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	u, err := scanUser(tx.QueryRow(ctx, `INSERT INTO users (id, display_name, password_hash, roles, station, must_change_password, created_at, created_by, updated_at, password_changed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $7) RETURNING `+userCols,
		in.ID, in.DisplayName, hash, rolesToStrings(in.Roles), in.Station, must, now, by))
	if isUnique(err) {
		return User{}, ErrExists
	}
	if err != nil {
		return User{}, err
	}
	if err := s.record(ctx, tx, by, action, u.ID, map[string]any{"roles": rolesToStrings(u.Roles), "station": u.Station}, c.IP); err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

// activeAdminsExcept says how many active administrators there are besides id. Its caller has
// locked them all (see lock), so the answer cannot change before the transaction ends.
func activeAdminsExcept(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE 'admin' = ANY (roles) AND NOT disabled AND id <> $1`, id).Scan(&n)
	return n, err
}

func hasRole(roles []rbac.Role, r rbac.Role) bool {
	for _, q := range roles {
		if q == r {
			return true
		}
	}
	return false
}

// lock reads an account and locks its row together with the rows of every administrator, always
// in the order of their ids. Two administrators who disable each other at once would otherwise
// each lock their target first and then wait for the other's: a deadlock. In one order, the
// second waits for the first to finish and then sees what it did, so the last administrator
// cannot be removed by two requests that each saw another one left.
func (s *Service) lock(ctx context.Context, tx pgx.Tx, id string) (User, error) {
	id = normalizeID(id)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 OR 'admin' = ANY (roles) ORDER BY id FOR UPDATE`, id); err != nil {
		return User{}, err
	}
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// UpdateUser replaces the name, roles and station of an account. Nobody changes their own roles,
// and the last administrator keeps the role.
func (s *Service) UpdateUser(ctx context.Context, by, id string, in UserUpdate, c Client) (User, error) {
	name, roles, station, err := validateEditable(in.DisplayName, in.Roles, in.Station)
	if err != nil {
		return User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	before, err := s.lock(ctx, tx, id)
	if err != nil {
		return User{}, err
	}
	if before.ID == by && !sameRoles(before.Roles, roles) {
		return User{}, ErrSelf
	}
	if hasRole(before.Roles, rbac.Admin) && !before.Disabled && !hasRole(roles, rbac.Admin) {
		n, err := activeAdminsExcept(ctx, tx, before.ID)
		if err != nil {
			return User{}, err
		}
		if n == 0 {
			return User{}, ErrLastAdmin
		}
	}
	u, err := scanUser(tx.QueryRow(ctx, `UPDATE users SET display_name = $2, roles = $3, station = $4, updated_at = $5 WHERE id = $1 RETURNING `+userCols,
		before.ID, name, rolesToStrings(roles), station, s.now()))
	if err != nil {
		return User{}, err
	}
	if err := s.record(ctx, tx, by, "user.update", u.ID, map[string]any{
		"roles_before": rolesToStrings(before.Roles), "roles_after": rolesToStrings(u.Roles),
		"station_before": before.Station, "station_after": u.Station, "name_changed": before.DisplayName != u.DisplayName,
	}, c.IP); err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

// SetDisabled turns an account off or on. Turning it off ends its sessions at once.
func (s *Service) SetDisabled(ctx context.Context, by, id string, disabled bool, c Client) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	before, err := s.lock(ctx, tx, id)
	if err != nil {
		return User{}, err
	}
	if disabled && before.ID == by {
		return User{}, ErrSelf
	}
	if disabled && !before.Disabled && hasRole(before.Roles, rbac.Admin) {
		n, err := activeAdminsExcept(ctx, tx, before.ID)
		if err != nil {
			return User{}, err
		}
		if n == 0 {
			return User{}, ErrLastAdmin
		}
	}
	now := s.now()
	u, err := scanUser(tx.QueryRow(ctx, `UPDATE users SET disabled = $2, updated_at = $3 WHERE id = $1 RETURNING `+userCols, before.ID, disabled, now))
	if err != nil {
		return User{}, err
	}
	ended := int64(0)
	if disabled {
		tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, u.ID, now)
		if err != nil {
			return User{}, err
		}
		ended = tag.RowsAffected()
	}
	action := "user.enable"
	if disabled {
		action = "user.disable"
	}
	if err := s.record(ctx, tx, by, action, u.ID, map[string]any{"sessions_ended": ended}, c.IP); err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

// ResetPassword gives an account a new random temporary password, returned once. The person
// must change it at the next sign-in, and every session of the account ends. It also clears a
// lock. Nobody resets their own: that is ChangePassword, which needs the current one.
func (s *Service) ResetPassword(ctx context.Context, by, id string, c Client) (string, error) {
	temp, err := NewTemporaryPassword()
	if err != nil {
		return "", err
	}
	hash, err := s.hash(ctx, temp)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	u, err := s.lock(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if u.ID == by {
		return "", ErrSelf
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, must_change_password = true, failed_attempts = 0, locked_until = NULL, password_changed_at = $3, updated_at = $3 WHERE id = $1`,
		u.ID, hash, now); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, u.ID, now)
	if err != nil {
		return "", err
	}
	if err := s.record(ctx, tx, by, "user.reset_password", u.ID, map[string]any{"sessions_ended": tag.RowsAffected()}, c.IP); err != nil {
		return "", err
	}
	return temp, tx.Commit(ctx)
}

// SetPassword sets a password chosen by whoever runs the command line, for the case where the
// only administrator is locked out or has forgotten the password. It follows the policy, clears
// the lock and ends the sessions.
func (s *Service) SetPassword(ctx context.Context, id, password string) error {
	hash := ""
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	u, err := s.lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := CheckPassword(password, u.ID, u.DisplayName); err != nil {
		return err
	}
	if hash, err = s.hash(ctx, password); err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, must_change_password = false, failed_attempts = 0, locked_until = NULL, password_changed_at = $3, updated_at = $3 WHERE id = $1`,
		u.ID, hash, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, u.ID, now); err != nil {
		return err
	}
	if err := s.record(ctx, tx, "command-line", "user.set_password", u.ID, nil, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Unlock clears the lock of an account.
func (s *Service) Unlock(ctx context.Context, by, id string, c Client) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := s.lock(ctx, tx, id); err != nil {
		return User{}, err
	}
	u, err := scanUser(tx.QueryRow(ctx, `UPDATE users SET failed_attempts = 0, locked_until = NULL, updated_at = $2 WHERE id = $1 RETURNING `+userCols, normalizeID(id), s.now()))
	if err != nil {
		return User{}, err
	}
	if err := s.record(ctx, tx, by, "user.unlock", u.ID, nil, c.IP); err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

// ListUsers returns every account, with how many live sessions each has.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	now := s.now()
	rows, err := s.pool.Query(ctx, `SELECT `+cols("u")+`,
		(SELECT count(*) FROM sessions s WHERE s.user_id = u.id AND s.revoked_at IS NULL AND s.idle_expires_at > $1 AND s.absolute_expires_at > $1)
		FROM users u ORDER BY u.id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var n int
		u, err := scanUser(rows, &n)
		if err != nil {
			return nil, err
		}
		u.ActiveSessions = n
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser returns one account.
func (s *Service) GetUser(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, normalizeID(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Sessions returns the live sessions of an account, newest first.
func (s *Service) Sessions(ctx context.Context, id string) ([]Session, error) {
	now := s.now()
	rows, err := s.pool.Query(ctx, `SELECT id, created_at, last_seen_at, idle_expires_at, absolute_expires_at, ip, user_agent FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND idle_expires_at > $2 AND absolute_expires_at > $2 ORDER BY id DESC`, normalizeID(id), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.LastSeenAt, &x.IdleExpiresAt, &x.AbsoluteExpiresAt, &x.IP, &x.UserAgent); err != nil {
			return nil, err
		}
		x.User.ID = normalizeID(id)
		out = append(out, x)
	}
	return out, rows.Err()
}

// RevokeSessions ends every live session of an account except keep (0 keeps none), and returns
// how many it ended.
func (s *Service) RevokeSessions(ctx context.Context, by, id string, keep int64, c Client) (int, error) {
	id = normalizeID(id)
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, id, keep, s.now())
	if err != nil {
		return 0, err
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		if err := s.record(ctx, s.pool, by, "session.revoke_all", id, map[string]any{"count": n}, c.IP); err != nil {
			return n, err
		}
	}
	return n, nil
}

// RevokeSession ends one session of an account.
func (s *Service) RevokeSession(ctx context.Context, by, id string, session int64, c Client) error {
	id = normalizeID(id)
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, session, id, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.record(ctx, s.pool, by, "session.revoke", id, map[string]any{"session": session}, c.IP)
}

// Events returns the newest lines of the trail, those about one account when subject is given.
func (s *Service) Events(ctx context.Context, subject string, limit int) ([]Event, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, at, actor, action, subject, detail::text, ip FROM account_events`
	args := []any{limit}
	if subject != "" {
		q += ` WHERE subject = $2 OR actor = $2`
		args = append(args, normalizeID(subject))
	}
	q += ` ORDER BY id DESC LIMIT $1`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Subject, &e.Detail, &e.IP); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purge deletes sessions that ended more than thirty days ago and returns how many.
func (s *Service) Purge(ctx context.Context) (int, error) {
	cutoff := s.now().Add(-30 * 24 * time.Hour)
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE absolute_expires_at < $1 OR idle_expires_at < $1 OR revoked_at < $1`, cutoff)
	return int(tag.RowsAffected()), err
}

// truncate shortens text that came from a client to n characters and makes it fit a PostgreSQL
// text column: valid UTF-8 and no NUL, which the database refuses.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// sameRoles reports whether two lists hold the same roles, whatever their order.
func sameRoles(a, b []rbac.Role) bool {
	if len(a) != len(b) {
		return false
	}
	for _, r := range a {
		if !hasRole(b, r) {
			return false
		}
	}
	return true
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
