package orderservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// AccountsOptions are the settings of the sign-in part of the API.
type AccountsOptions struct {
	// CookieName is the name of the session cookie; default "kitchen_session".
	CookieName string
	// SecureCookies marks the cookie Secure even when the request did not arrive over TLS: the
	// service listens on plain HTTP behind a proxy that does the TLS. Without it the cookie is
	// Secure only for a request that came in over TLS itself.
	SecureCookies bool
	// MaxLifetime is how long the cookie is kept by the browser; it should be the longest a
	// session can last (accounts.Config.MaxLifetime). Default 7 days.
	MaxLifetime time.Duration
	// LoginBurst and LoginPeriod limit sign-in attempts per address: a burst of LoginBurst
	// that refills over LoginPeriod. Default 30 per minute, enough for a shift that starts
	// together behind one address, too few for guessing.
	LoginBurst  int
	LoginPeriod time.Duration
	Now         func() time.Time
}

// APIOption changes how NewAPI builds the handler.
type APIOption func(*API)

// WithAccounts turns on signing in with a password, sessions, the account screens' API and the
// administration of users. If NewAPI was given no Authenticator it uses SessionAuth.
func WithAccounts(acc *accounts.Service, o AccountsOptions) APIOption {
	return func(a *API) {
		if o.CookieName == "" {
			o.CookieName = "kitchen_session"
		}
		if o.MaxLifetime <= 0 {
			o.MaxLifetime = 7 * 24 * time.Hour
		}
		if o.LoginBurst <= 0 {
			o.LoginBurst = 30
		}
		if o.LoginPeriod <= 0 {
			o.LoginPeriod = time.Minute
		}
		a.acc, a.opts = acc, o
		a.login = newLimiter(o.LoginBurst, o.LoginPeriod, o.Now)
		if a.authn == nil {
			a.authn = SessionAuth(acc, o.CookieName)
		}
	}
}

func (a *API) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", a.signIn)
	mux.HandleFunc("POST /api/logout", a.identified(a.signOut))
	mux.HandleFunc("POST /api/me/password", a.identified(a.changePassword))
	mux.HandleFunc("GET /api/me/sessions", a.authID(a.mySessions))
	mux.HandleFunc("POST /api/me/sessions/revoke-others", a.authID(a.revokeOtherSessions))
	mux.HandleFunc("POST /api/me/sessions/{id}/revoke", a.authID(a.revokeMySession))

	mux.HandleFunc("GET /api/admin/roles", a.admin(a.roles))
	mux.HandleFunc("GET /api/admin/users", a.admin(a.listUsers))
	mux.HandleFunc("POST /api/admin/users", a.admin(a.createUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", a.admin(a.updateUser))
	mux.HandleFunc("POST /api/admin/users/{id}/disable", a.admin(a.setDisabled(true)))
	mux.HandleFunc("POST /api/admin/users/{id}/enable", a.admin(a.setDisabled(false)))
	mux.HandleFunc("POST /api/admin/users/{id}/unlock", a.admin(a.unlockUser))
	mux.HandleFunc("POST /api/admin/users/{id}/reset-password", a.admin(a.resetPassword))
	mux.HandleFunc("POST /api/admin/users/{id}/revoke-sessions", a.admin(a.revokeUserSessions))
	mux.HandleFunc("GET /api/admin/users/{id}/sessions", a.admin(a.userSessions))
	mux.HandleFunc("GET /api/admin/events", a.admin(a.accountEvents))
}

// admin is authID for the handlers that need the permission to manage users.
func (a *API) admin(f func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return a.authID(func(w http.ResponseWriter, r *http.Request, id Identity) {
		if !id.Actor.Can(rbac.ManageUsers) {
			a.fail(w, fmt.Errorf("%w: needs %s", ErrNotAllowed, rbac.ManageUsers))
			return
		}
		f(w, r, id)
	})
}

// failAccounts answers an error of the accounts package.
func (a *API) failAccounts(w http.ResponseWriter, err error) {
	var locked *accounts.LockedError
	var invalid *accounts.InvalidError
	var weak *accounts.PasswordError
	body := errorBody{Error: strings.TrimPrefix(err.Error(), "accounts: ")}
	var code int
	switch {
	case errors.As(err, &locked):
		wait := time.Until(locked.Until)
		if wait < time.Second {
			wait = time.Second
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		body.Code, code, body.RetryAfter = "locked", http.StatusTooManyRequests, int(wait.Seconds())+1
		body.Error = fmt.Sprintf("Too many wrong passwords. Try again in %d minutes, or ask an administrator to unlock the account.", int(wait.Minutes())+1)
	case errors.Is(err, accounts.ErrBadCredentials):
		body.Code, code = "bad_credentials", http.StatusUnauthorized
		body.Error = "Wrong login name or password."
	case errors.Is(err, accounts.ErrNotFound):
		body.Code, code = "not_found", http.StatusNotFound
	case errors.Is(err, accounts.ErrExists), errors.Is(err, accounts.ErrSelf), errors.Is(err, accounts.ErrLastAdmin):
		body.Code, code = "conflict", http.StatusConflict
	case errors.As(err, &weak):
		body.Code, code = "weak_password", http.StatusBadRequest
	case errors.As(err, &invalid):
		body.Code, code = "invalid", http.StatusBadRequest
	default:
		a.logger.Error("an account request failed", "err", err)
		w.Header().Set("Retry-After", "2")
		body.Code, code = "unavailable", http.StatusServiceUnavailable
		body.Error = "the accounts database did not answer"
	}
	writeJSON(w, code, body)
}

func (a *API) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: a.opts.CookieName, Value: value, Path: "/", MaxAge: maxAge,
		// The script of the page never needs to read the session, and so cannot give it away.
		HttpOnly: true,
		// A request from another site does not carry it, whatever that site's page does.
		SameSite: http.SameSiteStrictMode,
		Secure:   a.opts.SecureCookies || r.TLS != nil,
	})
}

// --- signing in and out ---------------------------------------------------------------------

type signInBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *API) signIn(w http.ResponseWriter, r *http.Request) {
	if !sameSite(r) {
		writeJSON(w, http.StatusForbidden, errorBody{Error: "this request was made by another site", Code: "cross_site"})
		return
	}
	c := clientOf(r)
	if ok, wait := a.login.allow(c.IP); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: "Too many attempts from this address. Wait a moment and try again.", Code: "rate_limited", RetryAfter: int(wait.Seconds()) + 1})
		return
	}
	var b signInBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	token, sess, err := a.acc.Login(r.Context(), b.Username, b.Password, c)
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	a.setSessionCookie(w, r, token, int(a.opts.MaxLifetime.Seconds()))
	writeJSON(w, http.StatusOK, a.meView(identityOf(sess)))
}

func (a *API) signOut(w http.ResponseWriter, r *http.Request, _ Identity) {
	if c, err := r.Cookie(a.opts.CookieName); err == nil {
		if err := a.acc.Logout(r.Context(), c.Value, clientOf(r)); err != nil {
			a.failAccounts(w, err)
			return
		}
	}
	a.setSessionCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

type passwordBody struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request, id Identity) {
	var b passwordBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.acc.ChangePassword(r.Context(), id.Actor.ID, id.Session, b.Current, b.New, clientOf(r)); err != nil {
		if errors.Is(err, accounts.ErrBadCredentials) { // not a 401: the person is signed in, and a 401 would sign them out
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "The current password is wrong.", Code: "wrong_password"})
			return
		}
		a.failAccounts(w, err)
		return
	}
	u, err := a.acc.GetUser(r.Context(), id.Actor.ID)
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	id.MustChangePassword = u.MustChangePassword
	writeJSON(w, http.StatusOK, a.meView(id))
}

// --- my sessions ----------------------------------------------------------------------------

type sessionView struct {
	ID        int64     `json:"id"`
	Current   bool      `json:"current"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IP        string    `json:"ip"`
	Agent     string    `json:"user_agent"`
}

func sessionViews(list []accounts.Session, current int64) []sessionView {
	out := make([]sessionView, 0, len(list))
	for _, s := range list {
		out = append(out, sessionView{ID: s.ID, Current: s.ID == current, CreatedAt: s.CreatedAt, LastSeen: s.LastSeenAt, ExpiresAt: s.ExpiresAt(), IP: s.IP, Agent: s.UserAgent})
	}
	return out
}

func (a *API) mySessions(w http.ResponseWriter, r *http.Request, id Identity) {
	list, err := a.acc.Sessions(r.Context(), id.Actor.ID)
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessionViews(list, id.Session)})
}

func (a *API) revokeOtherSessions(w http.ResponseWriter, r *http.Request, id Identity) {
	n, err := a.acc.RevokeSessions(r.Context(), id.Actor.ID, id.Actor.ID, id.Session, clientOf(r))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

func (a *API) revokeMySession(w http.ResponseWriter, r *http.Request, id Identity) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n == id.Session { // ending the one in use is signing out
		a.failAccounts(w, &accounts.InvalidError{Reason: "give the number of another session"})
		return
	}
	if err := a.acc.RevokeSession(r.Context(), id.Actor.ID, id.Actor.ID, n, clientOf(r)); err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": 1})
}

// --- administration -------------------------------------------------------------------------

type userView struct {
	ID                 string     `json:"id"`
	DisplayName        string     `json:"display_name"`
	Roles              []string   `json:"roles"`
	Station            string     `json:"station"`
	Disabled           bool       `json:"disabled"`
	MustChangePassword bool       `json:"must_change_password"`
	LockedUntil        *time.Time `json:"locked_until,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	CreatedBy          string     `json:"created_by"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	ActiveSessions     int        `json:"active_sessions"`
}

func userViewOf(u accounts.User, now time.Time) userView {
	roles := make([]string, len(u.Roles))
	for i, r := range u.Roles {
		roles[i] = string(r)
	}
	v := userView{ID: u.ID, DisplayName: u.DisplayName, Roles: roles, Station: u.Station, Disabled: u.Disabled,
		MustChangePassword: u.MustChangePassword, CreatedAt: u.CreatedAt, CreatedBy: u.CreatedBy, LastLoginAt: u.LastLoginAt, ActiveSessions: u.ActiveSessions}
	if u.LockedUntil != nil && u.LockedUntil.After(now) { // a lock that has run out is not shown
		v.LockedUntil = u.LockedUntil
	}
	return v
}

// roles lists the roles with what each may do, for the form that gives people roles.
func (a *API) roles(w http.ResponseWriter, _ *http.Request, _ Identity) {
	type roleView struct {
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	var out []roleView
	for _, r := range rbac.AllRoles() {
		var ps []string
		for _, p := range rbac.Permissions(r) {
			ps = append(ps, string(p))
		}
		out = append(out, roleView{Role: string(r), Permissions: ps})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out})
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, _ Identity) {
	list, err := a.acc.ListUsers(r.Context())
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	now := time.Now()
	views := make([]userView, 0, len(list))
	for _, u := range list {
		views = append(views, userViewOf(u, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views})
}

type newUserBody struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Station     string   `json:"station"`
}

func toRoles(in []string) []rbac.Role {
	out := make([]rbac.Role, len(in))
	for i, r := range in {
		out[i] = rbac.Role(r)
	}
	return out
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request, id Identity) {
	var b newUserBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	u, temp, err := a.acc.CreateUser(r.Context(), id.Actor.ID, accounts.NewUser{ID: b.ID, DisplayName: b.DisplayName, Roles: toRoles(b.Roles), Station: b.Station}, clientOf(r))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	// The temporary password is in this answer and nowhere else: it is not stored in the clear.
	writeJSON(w, http.StatusCreated, map[string]any{"user": userViewOf(u, time.Now()), "temporary_password": temp})
}

func (a *API) updateUser(w http.ResponseWriter, r *http.Request, id Identity) {
	var b newUserBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if b.ID != "" && !strings.EqualFold(b.ID, r.PathValue("id")) {
		a.failAccounts(w, &accounts.InvalidError{Reason: "a login name cannot be changed"})
		return
	}
	u, err := a.acc.UpdateUser(r.Context(), id.Actor.ID, r.PathValue("id"), accounts.UserUpdate{DisplayName: b.DisplayName, Roles: toRoles(b.Roles), Station: b.Station}, clientOf(r))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userViewOf(u, time.Now())})
}

func (a *API) setDisabled(disabled bool) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		u, err := a.acc.SetDisabled(r.Context(), id.Actor.ID, r.PathValue("id"), disabled, clientOf(r))
		if err != nil {
			a.failAccounts(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"user": userViewOf(u, time.Now())})
	}
}

func (a *API) unlockUser(w http.ResponseWriter, r *http.Request, id Identity) {
	u, err := a.acc.Unlock(r.Context(), id.Actor.ID, r.PathValue("id"), clientOf(r))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userViewOf(u, time.Now())})
}

func (a *API) resetPassword(w http.ResponseWriter, r *http.Request, id Identity) {
	temp, err := a.acc.ResetPassword(r.Context(), id.Actor.ID, r.PathValue("id"), clientOf(r))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	u, err := a.acc.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userViewOf(u, time.Now()), "temporary_password": temp})
}

func (a *API) revokeUserSessions(w http.ResponseWriter, r *http.Request, id Identity) {
	n, err := a.acc.RevokeSessions(r.Context(), id.Actor.ID, r.PathValue("id"), 0, clientOf(r))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

func (a *API) userSessions(w http.ResponseWriter, r *http.Request, _ Identity) {
	list, err := a.acc.Sessions(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessionViews(list, 0)})
}

type eventView struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Actor   string          `json:"actor"`
	Action  string          `json:"action"`
	Subject string          `json:"subject"`
	Detail  json.RawMessage `json:"detail"`
	IP      string          `json:"ip"`
}

func (a *API) accountEvents(w http.ResponseWriter, r *http.Request, _ Identity) {
	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 500 {
			a.failAccounts(w, &accounts.InvalidError{Reason: "limit is a number from 1 to 500"})
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	list, err := a.acc.Events(ctx, r.URL.Query().Get("subject"), limit)
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	out := make([]eventView, 0, len(list))
	for _, e := range list {
		out = append(out, eventView{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action, Subject: e.Subject, Detail: json.RawMessage(e.Detail), IP: e.IP})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
