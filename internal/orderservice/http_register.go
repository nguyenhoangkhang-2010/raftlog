package orderservice

import (
	"net/http"
	"strconv"
	"time"
)

// WithOpenRegistration lets a person make an account of their own at /api/register, with the customer role and
// nothing else. Without it the route answers that registration is closed, and an administrator makes every
// account. It needs WithAccounts.
func WithOpenRegistration() APIOption {
	return func(a *API) { a.registration = true }
}

type registerBody struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// register makes a customer's account and signs the person in. The request is limited per address, much more
// tightly than signing in: a sign-in is a guess at one account, a registration makes a new one.
func (a *API) register(w http.ResponseWriter, r *http.Request) {
	if !sameSite(r) {
		writeJSON(w, http.StatusForbidden, errorBody{Error: "this request was made by another site", Code: "cross_site"})
		return
	}
	if !a.registration {
		writeJSON(w, http.StatusForbidden, errorBody{Error: "Registration is closed.", Code: "registration_closed"})
		return
	}
	c := clientOf(r)
	if ok, wait := a.signups.allow(c.IP); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: "Too many attempts from this address. Wait a moment and try again.", Code: "rate_limited", RetryAfter: int(wait.Seconds()) + 1})
		return
	}
	var b registerBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if _, err := a.acc.Register(r.Context(), b.Username, b.DisplayName, b.Password, c); err != nil {
		a.failAccounts(w, err)
		return
	}
	token, sess, err := a.acc.Login(r.Context(), b.Username, b.Password, c)
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	a.setSessionCookie(w, r, token, int(a.opts.MaxLifetime.Seconds()))
	writeJSON(w, http.StatusCreated, a.meView(identityOf(sess)))
}

// signupLimit is how many accounts one address may make, and over how long the allowance refills.
const (
	defaultSignupBurst  = 5
	defaultSignupPeriod = time.Hour
)
