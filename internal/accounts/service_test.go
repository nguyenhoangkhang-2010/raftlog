package accounts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

const goodPassword = "correct horse battery"

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newService returns a service on an empty schema of the test database, and its clock.
func newService(t *testing.T) (*Service, *clock) {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctx, pool, Migrations()); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)}
	s, err := New(pool, Config{Hash: cheap, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func mk(t *testing.T, s *Service, id string, station string, roles ...rbac.Role) User {
	t.Helper()
	u, err := s.CreateWithPassword(ctx, "test", NewUser{ID: id, DisplayName: "Person " + id, Roles: roles, Station: station}, goodPassword)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return u
}

func login(t *testing.T, s *Service, id, password string) (string, Session) {
	t.Helper()
	tok, sess, err := s.Login(ctx, id, password, Client{IP: "10.0.0.1", UserAgent: "test"})
	if err != nil {
		t.Fatalf("login %s: %v", id, err)
	}
	return tok, sess
}

func actions(t *testing.T, s *Service, subject string) []string {
	t.Helper()
	ev, err := s.Events(ctx, subject, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(ev) - 1; i >= 0; i-- { // oldest first
		out = append(out, ev[i].Action)
	}
	return out
}

func count(actions []string, a string) int {
	n := 0
	for _, x := range actions {
		if x == a {
			n++
		}
	}
	return n
}

func TestASessionStartsAtLoginAndIsFoundByItsToken(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "thu-ngan-1", "", rbac.Cashier, rbac.Runner)
	tok, sess := login(t, s, "thu-ngan-1", goodPassword)
	if len(tok) != tokenLength {
		t.Fatalf("token %q has %d characters", tok, len(tok))
	}
	got, err := s.Authenticate(ctx, tok)
	if err != nil || got.ID != sess.ID || got.User.ID != "thu-ngan-1" {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	if a := got.User.Actor(); !a.Can(rbac.RecordPayment) || !a.Can(rbac.HandOver) || a.Can(rbac.ManageUsers) {
		t.Fatalf("actor %+v has the wrong permissions", a)
	}
	for _, bad := range []string{"", "x", tok[:42], tok + "a", strings.Repeat("A", tokenLength)} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("token %q gave %v", bad, err)
		}
	}
	// The database keeps a hash, not the token.
	var stored []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE id = $1`, sess.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 32 || strings.Contains(string(stored), tok) {
		t.Fatal("the session table holds something other than a hash of the token")
	}
}

func TestLoginNamesAreNotCaseSensitive(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "thu-ngan-1", "", rbac.Cashier)
	login(t, s, "  Thu-Ngan-1 ", goodPassword)
}

func TestAWrongPasswordAnUnknownNameAndADisabledAccountAnswerAlike(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	mk(t, s, "cashier-b", "", rbac.Cashier)
	if _, err := s.SetDisabled(ctx, "test", "cashier-b", true, Client{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, pw string }{
		{"cashier-a", "wrong password here"}, {"nobody-here", goodPassword}, {"cashier-b", goodPassword}, {"cashier-b", "wrong"},
		{"", ""}, {"a\x00b", goodPassword}, {"\xff\xfe", goodPassword}, {strings.Repeat("x", 500), goodPassword}, {"cashier-a", strings.Repeat("p", 5000)},
	} {
		_, _, err := s.Login(ctx, c.id, c.pw, Client{IP: "10.0.0.1", UserAgent: "ua\x00\xff" + strings.Repeat("y", 1000)})
		if !errors.Is(err, ErrBadCredentials) {
			t.Errorf("login %q/%q gave %v, want the one answer for all of them", c.id, c.pw, err)
		}
	}
	if _, err := s.Authenticate(ctx, "x"); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
	// A disabled account's right password is not counted as a failure: it cannot be locked by it.
	u, _ := s.GetUser(ctx, "cashier-b")
	if u.FailedAttempts != 1 { // only the "wrong" one above
		t.Fatalf("cashier-b has %d failed attempts, want 1", u.FailedAttempts)
	}
}

func TestFiveWrongPasswordsLockTheAccountForAWhile(t *testing.T) {
	s, clk := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	for i := 1; i <= 4; i++ {
		if _, _, err := s.Login(ctx, "cashier-a", "wrong password "+string(rune('a'+i)), Client{}); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, _, err := s.Login(ctx, "cashier-a", "wrong password z", Client{})
	var locked *LockedError
	if !errors.As(err, &locked) || !locked.Until.Equal(clk.now().Add(15*time.Minute)) {
		t.Fatalf("the fifth wrong password gave %v, want a lock for 15 minutes", err)
	}
	// Locked means locked: the right password does not get in either.
	if _, _, err := s.Login(ctx, "cashier-a", goodPassword, Client{}); !errors.As(err, &locked) {
		t.Fatalf("the right password during the lock gave %v", err)
	}
	clk.advance(14 * time.Minute)
	if _, _, err := s.Login(ctx, "cashier-a", goodPassword, Client{}); !errors.As(err, &locked) {
		t.Fatal("the lock ended early")
	}
	clk.advance(2 * time.Minute)
	login(t, s, "cashier-a", goodPassword)
	u, _ := s.GetUser(ctx, "cashier-a")
	if u.FailedAttempts != 0 || u.LockedUntil != nil {
		t.Fatalf("a good sign-in left %d failures and a lock %v", u.FailedAttempts, u.LockedUntil)
	}
	a := actions(t, s, "cashier-a")
	if count(a, "login.failure") != 4 || count(a, "login.locked") != 1 || count(a, "login.blocked") != 2 || count(a, "login.success") != 1 {
		t.Fatalf("the trail is %v", a)
	}
}

func TestAnAdministratorCanUnlockAnAccount(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	for i := 0; i < 5; i++ {
		_, _, _ = s.Login(ctx, "cashier-a", "wrong password here", Client{})
	}
	if _, _, err := s.Login(ctx, "cashier-a", goodPassword, Client{}); err == nil {
		t.Fatal("the account was not locked")
	}
	if _, err := s.Unlock(ctx, "boss-admin", "cashier-a", Client{}); err != nil {
		t.Fatal(err)
	}
	login(t, s, "cashier-a", goodPassword)
	if got := actions(t, s, "cashier-a"); count(got, "user.unlock") != 1 {
		t.Fatalf("trail %v", got)
	}
}

func TestSimultaneousWrongPasswordsAreCountedOneByOne(t *testing.T) {
	// The row of the account is locked while a password is checked, so twenty guesses at once are
	// five failures and a lock, not twenty failures that each saw "attempt 0".
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.Login(ctx, "cashier-a", "wrong password here", Client{})
		}()
	}
	wg.Wait()
	a := actions(t, s, "cashier-a")
	if count(a, "login.failure") != 4 || count(a, "login.locked") != 1 || count(a, "login.blocked") != 15 {
		t.Fatalf("failures %d, locks %d, blocked %d; want 4, 1 and 15", count(a, "login.failure"), count(a, "login.locked"), count(a, "login.blocked"))
	}
}

func TestASessionEndsWhenIdleAndAtItsAbsoluteAge(t *testing.T) {
	s, clk := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	tok, _ := login(t, s, "cashier-a", goodPassword)

	// Used within twelve hours it keeps going, past twelve hours since it began.
	for i := 0; i < 3; i++ {
		clk.advance(11 * time.Hour)
		if _, err := s.Authenticate(ctx, tok); err != nil {
			t.Fatalf("after %d x 11 hours of use: %v", i+1, err)
		}
	}
	clk.advance(13 * time.Hour)
	if _, err := s.Authenticate(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a session idle for 13 hours gave %v", err)
	}

	// However often it is used, it ends seven days after sign-in.
	tok, _ = login(t, s, "cashier-a", goodPassword)
	var last error
	for i := 0; i < 20 && last == nil; i++ {
		clk.advance(11 * time.Hour)
		_, last = s.Authenticate(ctx, tok)
	}
	if !errors.Is(last, ErrNoSession) {
		t.Fatalf("a session in use for 220 hours was still alive: %v", last)
	}
}

func TestTheIdleClockMovesOnlyOnceAMinute(t *testing.T) {
	s, clk := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	tok, sess := login(t, s, "cashier-a", goodPassword)
	clk.advance(20 * time.Second)
	a, _ := s.Authenticate(ctx, tok)
	if !a.LastSeenAt.Equal(sess.LastSeenAt) {
		t.Fatal("every request wrote to the database")
	}
	clk.advance(2 * time.Minute)
	b, _ := s.Authenticate(ctx, tok)
	if !b.LastSeenAt.After(sess.LastSeenAt) || !b.IdleExpiresAt.After(sess.IdleExpiresAt) {
		t.Fatalf("a request after two minutes did not move the idle clock: %+v", b)
	}
}

func TestRolesAndDisablingTakeEffectOnTheNextRequest(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	tok, _ := login(t, s, "cashier-a", goodPassword)
	if _, err := s.UpdateUser(ctx, "boss-admin", "cashier-a", UserUpdate{DisplayName: "Cashier A", Roles: []rbac.Role{rbac.Kitchen}, Station: "drinks"}, Client{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.User.Actor(); a.Can(rbac.RecordPayment) || !a.Can(rbac.StartTicket) || a.Station != "drinks" {
		t.Fatalf("the old session still has the old role: %+v", a)
	}
	if _, err := s.SetDisabled(ctx, "boss-admin", "cashier-a", true, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a disabled account's session still worked: %v", err)
	}
	if _, err := s.SetDisabled(ctx, "boss-admin", "cashier-a", false, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatal("enabling an account brought an old session back to life")
	}
	login(t, s, "cashier-a", goodPassword)
}

func TestLogoutEndsOnlyThatSession(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	t1, _ := login(t, s, "cashier-a", goodPassword)
	t2, _ := login(t, s, "cashier-a", goodPassword)
	if err := s.Logout(ctx, t1, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, t1); !errors.Is(err, ErrNoSession) {
		t.Fatal("the session survived its logout")
	}
	if _, err := s.Authenticate(ctx, t2); err != nil {
		t.Fatalf("logging out one session ended another: %v", err)
	}
	if err := s.Logout(ctx, t1, Client{}); err != nil || s.Logout(ctx, "garbage", Client{}) != nil {
		t.Fatal("logging out twice, or with garbage, is an error")
	}
}

func TestChangingThePassword(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	tok1, _ := login(t, s, "cashier-a", goodPassword)
	tok2, keep := login(t, s, "cashier-a", goodPassword)

	if err := s.ChangePassword(ctx, "cashier-a", keep.ID, "not the current one", "another good password", Client{}); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("a wrong current password gave %v", err)
	}
	var pe *PasswordError
	if err := s.ChangePassword(ctx, "cashier-a", keep.ID, goodPassword, "short", Client{}); !errors.As(err, &pe) {
		t.Fatalf("a weak new password gave %v", err)
	}
	if err := s.ChangePassword(ctx, "cashier-a", keep.ID, goodPassword, goodPassword, Client{}); !errors.As(err, &pe) {
		t.Fatalf("the same password again gave %v", err)
	}
	const next = "a much better password"
	if err := s.ChangePassword(ctx, "cashier-a", keep.ID, goodPassword, next, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok1); !errors.Is(err, ErrNoSession) {
		t.Fatal("another session survived a password change")
	}
	if _, err := s.Authenticate(ctx, tok2); err != nil {
		t.Fatal("the session that changed the password ended")
	}
	if _, _, err := s.Login(ctx, "cashier-a", goodPassword, Client{}); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("the old password still works")
	}
	login(t, s, "cashier-a", next)
	if a := actions(t, s, "cashier-a"); count(a, "password.change") != 1 || count(a, "password.wrong_current") != 1 {
		t.Fatalf("trail %v", a)
	}
}

func TestGuessingTheCurrentPasswordIsCountedAndLocks(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	var locked *LockedError
	var err error
	for i := 0; i < 5; i++ {
		err = s.ChangePassword(ctx, "cashier-a", 0, "wrong guess "+string(rune('a'+i)), "another good password", Client{})
	}
	if !errors.As(err, &locked) {
		t.Fatalf("five wrong current passwords gave %v, want a lock", err)
	}
}

func TestCreatingAnAccount(t *testing.T) {
	s, _ := newService(t)
	u, temp, err := s.CreateUser(ctx, "boss-admin", NewUser{ID: "Bep-Do-Uong", DisplayName: " Bếp đồ uống ", Roles: []rbac.Role{rbac.Kitchen}, Station: "Drinks"}, Client{IP: "10.0.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "bep-do-uong" || u.DisplayName != "Bếp đồ uống" || u.Station != "drinks" || !u.MustChangePassword || u.CreatedBy != "boss-admin" {
		t.Fatalf("created %+v", u)
	}
	_, sess := login(t, s, "bep-do-uong", temp)
	if !sess.User.MustChangePassword {
		t.Fatal("a temporary password does not have to be changed")
	}
	if err := s.ChangePassword(ctx, "bep-do-uong", sess.ID, temp, "my own long password", Client{}); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.GetUser(ctx, "bep-do-uong"); u.MustChangePassword {
		t.Fatal("the flag stayed after the change")
	}
	if _, _, err := s.CreateUser(ctx, "boss-admin", NewUser{ID: "BEP-DO-UONG", DisplayName: "x", Roles: []rbac.Role{rbac.Cashier}}, Client{}); !errors.Is(err, ErrExists) {
		t.Fatalf("a repeated name gave %v", err)
	}
}

func TestAnAccountWithBadFieldsIsRefused(t *testing.T) {
	s, _ := newService(t)
	cashier := []rbac.Role{rbac.Cashier}
	for name, in := range map[string]NewUser{
		"short id":                {ID: "ab", DisplayName: "x", Roles: cashier},
		"id with a space":         {ID: "a b c", DisplayName: "x", Roles: cashier},
		"id with capitals only":   {ID: "-abc", DisplayName: "x", Roles: cashier},
		"id too long":             {ID: strings.Repeat("a", 33), DisplayName: "x", Roles: cashier},
		"no name":                 {ID: "abc", DisplayName: "  ", Roles: cashier},
		"name too long":           {ID: "abc", DisplayName: strings.Repeat("n", 61), Roles: cashier},
		"name with a control":     {ID: "abc", DisplayName: "a\x07b", Roles: cashier},
		"no role":                 {ID: "abc", DisplayName: "x"},
		"unknown role":            {ID: "abc", DisplayName: "x", Roles: []rbac.Role{"owner"}},
		"kitchen without station": {ID: "abc", DisplayName: "x", Roles: []rbac.Role{rbac.Kitchen}},
		"station without kitchen": {ID: "abc", DisplayName: "x", Roles: cashier, Station: "drinks"},
		"bad station":             {ID: "abc", DisplayName: "x", Roles: []rbac.Role{rbac.Kitchen}, Station: "Dr1nks!"},
	} {
		_, _, err := s.CreateUser(ctx, "boss-admin", in, Client{})
		var ie *InvalidError
		if !errors.As(err, &ie) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if us, _ := s.ListUsers(ctx); len(us) != 0 {
		t.Fatalf("%d accounts were created from refused requests", len(us))
	}
}

func TestTheLastAdministratorStays(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "admin-one", "", rbac.Admin)
	mk(t, s, "admin-two", "", rbac.Admin)

	if _, err := s.SetDisabled(ctx, "admin-one", "admin-one", true, Client{}); !errors.Is(err, ErrSelf) {
		t.Fatalf("disabling yourself gave %v", err)
	}
	if _, err := s.UpdateUser(ctx, "admin-one", "admin-one", UserUpdate{DisplayName: "x", Roles: []rbac.Role{rbac.Manager}}, Client{}); !errors.Is(err, ErrSelf) {
		t.Fatalf("changing your own roles gave %v", err)
	}
	if _, err := s.UpdateUser(ctx, "admin-one", "admin-one", UserUpdate{DisplayName: "Renamed", Roles: []rbac.Role{rbac.Admin}}, Client{}); err != nil {
		t.Fatalf("changing your own name is allowed: %v", err)
	}
	if _, err := s.SetDisabled(ctx, "admin-one", "admin-two", true, Client{}); err != nil {
		t.Fatal(err)
	}
	// Only admin-one is left, so nobody else may turn it off or take the role away.
	if _, err := s.SetDisabled(ctx, "somebody", "admin-one", true, Client{}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the last admin gave %v", err)
	}
	if _, err := s.UpdateUser(ctx, "somebody", "admin-one", UserUpdate{DisplayName: "x", Roles: []rbac.Role{rbac.Cashier}}, Client{}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last admin gave %v", err)
	}
	// With another active one it is fine again.
	if _, err := s.SetDisabled(ctx, "admin-one", "admin-two", false, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDisabled(ctx, "admin-two", "admin-one", true, Client{}); err != nil {
		t.Fatalf("disabling an admin while another is active: %v", err)
	}
}

func TestTwoAdministratorsCannotRemoveEachOtherAtOnce(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "admin-one", "", rbac.Admin)
	mk(t, s, "admin-two", "", rbac.Admin)
	// One round is over in a few milliseconds and may not overlap at all, so go round many
	// times: without the ordered locks of lock() this fails within a handful of rounds.
	for round := 0; round < 60; round++ {
		if _, err := s.pool.Exec(ctx, `UPDATE users SET disabled = false`); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, pair := range [][2]string{{"admin-one", "admin-two"}, {"admin-two", "admin-one"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.SetDisabled(ctx, pair[0], pair[1], true, Client{})
			}()
		}
		wg.Wait()
		// Exactly one of them gets through; the other finds that its target is now the last
		// administrator. A raw database error (a deadlock) would be a third kind of answer.
		ok, last := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLastAdmin):
				last++
			default:
				t.Errorf("round %d: unexpected answer: %v", round, err)
			}
		}
		if ok != 1 || last != 1 {
			t.Fatalf("round %d: %d succeeded and %d were refused as the last administrator; want one of each", round, ok, last)
		}
	}
}

func TestResettingAPassword(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	tok, _ := login(t, s, "cashier-a", goodPassword)
	for i := 0; i < 5; i++ {
		_, _, _ = s.Login(ctx, "cashier-a", "wrong password here", Client{})
	}
	if _, err := s.ResetPassword(ctx, "cashier-a", "cashier-a", Client{}); !errors.Is(err, ErrSelf) {
		t.Fatalf("resetting your own password gave %v", err)
	}
	temp, err := s.ResetPassword(ctx, "boss-admin", "cashier-a", Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatal("a reset left the old session alive")
	}
	if _, _, err := s.Login(ctx, "cashier-a", goodPassword, Client{}); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("the old password survived a reset")
	}
	_, sess := login(t, s, "cashier-a", temp) // the lock was cleared too
	if !sess.User.MustChangePassword {
		t.Fatal("a reset password does not have to be changed")
	}
	if _, err := s.ResetPassword(ctx, "boss-admin", "nobody-here", Client{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resetting an unknown account gave %v", err)
	}
}

func TestTheCommandLineCanSetAPasswordAndClearALock(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "admin-one", "", rbac.Admin)
	for i := 0; i < 5; i++ {
		_, _, _ = s.Login(ctx, "admin-one", "wrong password here", Client{})
	}
	var pe *PasswordError
	if err := s.SetPassword(ctx, "admin-one", "weak"); !errors.As(err, &pe) {
		t.Fatalf("a weak password from the command line gave %v", err)
	}
	if err := s.SetPassword(ctx, "admin-one", "recovered long password"); err != nil {
		t.Fatal(err)
	}
	_, sess := login(t, s, "admin-one", "recovered long password")
	if sess.User.MustChangePassword {
		t.Fatal("the password chosen at the command line must be changed at once")
	}
}

func TestSessionsOfAnAccountAreListedAndRevoked(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	_, s1 := login(t, s, "cashier-a", goodPassword)
	t2, s2 := login(t, s, "cashier-a", goodPassword)
	login(t, s, "cashier-a", goodPassword)
	list, err := s.Sessions(ctx, "cashier-a")
	if err != nil || len(list) != 3 || list[0].ID < list[2].ID {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	us, _ := s.ListUsers(ctx)
	if len(us) != 1 || us[0].ActiveSessions != 3 {
		t.Fatalf("ListUsers = %+v", us)
	}
	if err := s.RevokeSession(ctx, "boss-admin", "cashier-a", s1.ID, Client{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "boss-admin", "cashier-a", s1.ID, Client{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice gave %v", err)
	}
	if err := s.RevokeSession(ctx, "boss-admin", "someone-else", s2.ID, Client{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a session through another account gave %v", err)
	}
	n, err := s.RevokeSessions(ctx, "cashier-a", "cashier-a", s2.ID, Client{})
	if err != nil || n != 1 {
		t.Fatalf("RevokeSessions = %d, %v", n, err)
	}
	if _, err := s.Authenticate(ctx, t2); err != nil {
		t.Fatal("the session that was kept ended")
	}
}

func TestOldSessionsArePurged(t *testing.T) {
	s, clk := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	login(t, s, "cashier-a", goodPassword)
	if n, _ := s.Purge(ctx); n != 0 {
		t.Fatalf("purged %d live sessions", n)
	}
	clk.advance(45 * 24 * time.Hour)
	if n, err := s.Purge(ctx); err != nil || n != 1 {
		t.Fatalf("Purge = %d, %v", n, err)
	}
}

func TestTheTrailCannotBeChanged(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	login(t, s, "cashier-a", goodPassword)
	for _, q := range []string{
		`UPDATE account_events SET action = 'nothing happened'`,
		`DELETE FROM account_events`,
		`TRUNCATE account_events`,
	} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Errorf("%q was allowed", q)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM account_events`).Scan(&n); err != nil || n < 2 {
		t.Fatalf("%d events left, %v", n, err)
	}
}

func TestAChangeAndItsLineInTheTrailAreOneTransaction(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "cashier-a", "", rbac.Cashier)
	// Make the trail refuse a line: the change must not be left behind without it.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE account_events ADD CONSTRAINT no_unlock CHECK (action <> 'user.unlock')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET failed_attempts = 3 WHERE id = 'cashier-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unlock(ctx, "boss-admin", "cashier-a", Client{}); err == nil {
		t.Fatal("an unlock was reported although its line could not be written")
	}
	if u, _ := s.GetUser(ctx, "cashier-a"); u.FailedAttempts != 3 {
		t.Fatal("the unlock was applied although its line could not be written")
	}
}

func TestTheDatabaseKnowsTheSameRolesAsTheCode(t *testing.T) {
	s, _ := newService(t)
	for i, r := range rbac.AllRoles() {
		station := ""
		if r == rbac.Kitchen {
			station = "drinks"
		}
		mk(t, s, "user-"+string(rune('a'+i)), station, r)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET roles = ARRAY['owner'] WHERE id = 'user-a'`); err == nil {
		t.Fatal("the database accepted a role that the code does not know")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET station = 'drinks' WHERE id = 'user-a'`); err == nil {
		t.Fatal("the database accepted a station for someone who is not a kitchen worker")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET station = '' WHERE roles @> ARRAY['kitchen']`); err == nil {
		t.Fatal("the database accepted a kitchen worker with no station")
	}
}

func TestAPersonWhoSignsUpGetsACustomerAccountAndNothingMore(t *testing.T) {
	s, _ := newService(t)
	u, err := s.Register(ctx, "  Linh.Tran  ", "Linh Trần", goodPassword, Client{IP: "203.0.113.9"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "linh.tran" || u.DisplayName != "Linh Trần" || len(u.Roles) != 1 || u.Roles[0] != rbac.Customer || u.Station != "" || u.MustChangePassword || u.Disabled || u.CreatedBy != "linh.tran" {
		t.Fatalf("user: %+v", u)
	}
	// the password is theirs: they sign in with it
	if _, sess, err := s.Login(ctx, "linh.tran", goodPassword, Client{}); err != nil || sess.User.ID != "linh.tran" {
		t.Fatalf("login: %v", err)
	}
	// the trail says that the person made the account, from where
	events, err := s.Events(ctx, "linh.tran", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == "user.register" && e.Actor == "linh.tran" && e.IP == "203.0.113.9" {
			found = true
		}
		if e.Action == "user.create" {
			t.Errorf("a registration was written as an account made by an administrator: %+v", e)
		}
	}
	if !found {
		t.Fatalf("no registration in the trail: %+v", events)
	}
}

func TestRegisteringFollowsTheRulesOfEveryAccount(t *testing.T) {
	s, _ := newService(t)
	mk(t, s, "taken-name", "", rbac.Customer)
	for name, c := range map[string]struct{ id, name, password string }{
		"a login that is taken":          {"taken-name", "A", goodPassword},
		"the same in capitals":           {"TAKEN-NAME", "A", goodPassword},
		"a short login":                  {"ab", "A", goodPassword},
		"a login with a space":           {"two words", "A", goodPassword},
		"no name":                        {"newcomer", "  ", goodPassword},
		"a name with a control":          {"newcomer", "A\x00B", goodPassword},
		"a weak password":                {"newcomer", "A", "short"},
		"a password with the login name": {"newcomer", "A", "newcomer-is-me-1"},
	} {
		if _, err := s.Register(ctx, c.id, c.name, c.password, Client{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.Register(ctx, "taken-name", "A", goodPassword, Client{}); !errors.Is(err, ErrExists) {
		t.Errorf("a taken login: %v", err)
	}
	if users, _ := s.ListUsers(ctx); len(users) != 1 {
		t.Errorf("a refused registration left %d accounts", len(users))
	}
}
