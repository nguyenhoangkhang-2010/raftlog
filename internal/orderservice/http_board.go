package orderservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// WithPublicBoard lets anyone who can reach the service read the call board (the numbers of the
// orders being made and of those that are ready) without signing in, so that a screen in the shop
// can show it. Without it, the board is for people who are signed in and may see all orders.
func WithPublicBoard(on bool) APIOption {
	return func(a *API) { a.publicBoard = on }
}

// maxBoardStreams bounds the open streams of the call board, so that a public board cannot be used
// to hold the service's connections.
const maxBoardStreams = 100

func (a *API) boardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/board", a.boardGuard(a.boardNow))
	mux.HandleFunc("GET /api/board/stream", a.boardGuard(a.boardStream))
}

// boardGuard lets everyone through when the board is public, and otherwise those who are signed in
// and may see every order.
func (a *API) boardGuard(f http.HandlerFunc) http.HandlerFunc {
	if a.publicBoard {
		return f
	}
	return a.auth(func(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
		if !actor.Can(rbac.ViewAllOrders) {
			a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewAllOrders))
			return
		}
		f(w, r)
	})
}

func (a *API) boardNow(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.Board())
}

// boardStream says "board" whenever an order changes; the screen then asks for the board again.
func (a *API) boardStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		a.fail(w, errors.New("streaming is not supported here"))
		return
	}
	if a.boardStreams.Add(1) > maxBoardStreams {
		a.boardStreams.Add(-1)
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "too many screens are listening", Code: "unavailable"})
		return
	}
	defer a.boardStreams.Add(-1)
	sub := a.svc.SubscribeBoard()
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 2000\n\n")
	fl.Flush()

	tick := time.NewTicker(streamKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !a.publicBoard && !a.stillMayWatch(r) {
				_, _ = io.WriteString(w, "event: reset\ndata: {}\n\n")
				fl.Flush()
				return
			}
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			fl.Flush()
		case ch, open := <-sub.C:
			if !open {
				_, _ = io.WriteString(w, "event: reset\ndata: {}\n\n")
				fl.Flush()
				return
			}
			_, _ = fmt.Fprintf(w, "event: board\ndata: {\"offset\":%d}\n\n", ch.Offset)
			fl.Flush()
		}
	}
}

// stillMayWatch asks again who the caller is: a stream lasts for hours, and a person who was
// signed out or lost the right to see orders must not go on watching.
func (a *API) stillMayWatch(r *http.Request) bool {
	id, err := a.authn.Authenticate(r.WithContext(context.WithValue(r.Context(), peekKey{}, true)))
	return err == nil && id.Actor.Can(rbac.ViewAllOrders)
}
