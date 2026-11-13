package orderservice

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// Identity is who is calling and how they proved it.
type Identity struct {
	Actor       rbac.Actor
	DisplayName string
	// Session is the session of a signed-in person; 0 for a caller that used a token of the
	// users file.
	Session int64
	// MustChangePassword is set while the password is a temporary one: nothing but changing it,
	// reading who you are and signing out is allowed until it is changed.
	MustChangePassword bool
	// ExpiresAt is when the session ends if it is not used again; zero if it does not expire.
	ExpiresAt time.Time
}

// Authenticator finds out who is calling. It returns ErrUnauthenticated for a request that is not
// from a known person, ErrCrossSite for one that a browser sent on behalf of another site, and
// any other error when it could not tell (a database that does not answer).
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// peekKey marks a context whose request only checks the session, without using it (see
// accounts.Service.Peek).
type peekKey struct{}

// Errors of authentication.
var (
	ErrUnauthenticated = errors.New("orderservice: not signed in")
	ErrCrossSite       = errors.New("orderservice: a request made by another site")
)

// --- tokens of the users file --------------------------------------------------------------

type tokenAuth struct{ users *Users }

// TokenAuth authenticates a bearer token against the users file. It is for tests and for trying
// the service without a database: a token never expires and is as good as the file it is in.
func TokenAuth(u *Users) Authenticator { return tokenAuth{u} }

func (t tokenAuth) Authenticate(r *http.Request) (Identity, error) {
	a, ok := t.users.Authenticate(r)
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Actor: a, DisplayName: a.ID}, nil
}

// --- sessions of the accounts database -----------------------------------------------------

type sessionAuth struct {
	acc    *accounts.Service
	cookie string
}

// SessionAuth authenticates the session cookie of a browser against the accounts database. The
// account is read at every request, so a change of roles or a disabled account counts at once.
//
// A cookie is sent by the browser whoever asked for the request, so for a request that changes
// something it also checks that the request was made by this site (see sameSite).
func SessionAuth(acc *accounts.Service, cookieName string) Authenticator {
	return sessionAuth{acc: acc, cookie: cookieName}
}

func (s sessionAuth) Authenticate(r *http.Request) (Identity, error) {
	c, err := r.Cookie(s.cookie)
	if err != nil || c.Value == "" {
		return Identity{}, ErrUnauthenticated
	}
	if !safeMethod(r.Method) && !sameSite(r) {
		return Identity{}, ErrCrossSite
	}
	check := s.acc.Authenticate
	if r.Context().Value(peekKey{}) != nil {
		check = s.acc.Peek
	}
	sess, err := check(r.Context(), c.Value)
	switch {
	case errors.Is(err, accounts.ErrNoSession):
		return Identity{}, ErrUnauthenticated
	case err != nil:
		return Identity{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return identityOf(sess), nil
}

func identityOf(s accounts.Session) Identity {
	return Identity{Actor: s.User.Actor(), DisplayName: s.User.DisplayName, Session: s.ID, MustChangePassword: s.User.MustChangePassword, ExpiresAt: s.ExpiresAt()}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameSite reports whether a request that changes something was made by a page of this site.
// Browsers say so themselves: Sec-Fetch-Site is "same-origin" for a request from our own page and
// "none" for one the person typed or bookmarked; Origin, which older browsers send with a POST,
// must name this host. A client that is not a browser sends neither, and a cookie that it holds
// it was given on purpose. This is the second line; the first is SameSite=Strict on the cookie.
func sameSite(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	return true
}

// client says where a request came from. Only the address of the connection is used: a header
// such as X-Forwarded-For can be written by anyone who reaches the service, so trusting it would
// let a stranger choose which address the limits count against.
func clientOf(r *http.Request) accounts.Client {
	return accounts.Client{IP: remoteIP(r), UserAgent: r.UserAgent()}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- a limit on attempts per address --------------------------------------------------------

// limiter lets each address make a number of attempts in a period. It is a token bucket: each
// address has a bucket of attempts that fills at a steady rate, so a burst is allowed and a
// steady stream is not. It lives in memory, so each instance of the service counts alone.
type limiter struct {
	burst  float64
	perSec float64
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(burst int, period time.Duration, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{burst: float64(burst), perSec: float64(burst) / period.Seconds(), now: now, buckets: map[string]*bucket{}}
}

// maxBuckets bounds the memory of the limiter: when it is reached, the addresses whose bucket is
// full again (that is, those that have not tried for a while) are forgotten.
const maxBuckets = 4096

// allow takes an attempt for the address, or says how long to wait for the next.
func (l *limiter) allow(key string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			for k, o := range l.buckets {
				if o.tokens+now.Sub(o.last).Seconds()*l.perSec >= l.burst {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= maxBuckets { // every address is busy: refuse the new one
				return false, time.Second
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.perSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
	}
	b.tokens--
	return true, 0
}
