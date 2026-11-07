package waitingroom

import (
	"bytes"
	"context"
	_ "embed" // the page is part of the binary
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

//go:embed web/index.html
var indexHTML []byte

// maxBodyBytes bounds the body of a join request.
const maxBodyBytes = 4 << 10

// HandlerOptions configures the HTTP handler.
type HandlerOptions struct {
	// Keepalive is how often a watching connection gets a comment line when
	// nothing changed, so that proxies do not close it as idle. Must be positive.
	Keepalive time.Duration
}

// NewHandler returns the HTTP interface of a room:
//
//	GET  /                 the web page
//	POST /api/join         {"ticket": "...", "user": "..."} -> status
//	GET  /api/status?ticket=ID   status of a ticket
//	GET  /api/watch?ticket=ID    the same as a stream of server-sent events
//	GET  /api/stats        totals of the room
//	GET  /healthz          200 once the room has read both logs
func NewHandler(room *Room, opts HandlerOptions) (http.Handler, error) {
	if opts.Keepalive <= 0 {
		return nil, errors.New("waitingroom: Keepalive must be positive")
	}
	h := &handler{room: room, keepalive: opts.Keepalive}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("POST /api/join", h.join)
	mux.HandleFunc("GET /api/status", h.status)
	mux.HandleFunc("GET /api/watch", h.watch)
	mux.HandleFunc("GET /api/stats", h.stats)
	mux.HandleFunc("GET /healthz", h.health)
	return mux, nil
}

type handler struct {
	room      *Room
	keepalive time.Duration
}

func (h *handler) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrInvalidTicket), errors.Is(err, ErrInvalidUser):
		code = http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	case errors.Is(err, client.ErrUnavailable), errors.Is(err, client.ErrClosed):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

type joinRequest struct {
	Ticket string `json:"ticket"`
	User   string `json:"user"`
}

func (h *handler) join(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body must be a JSON object with \"ticket\" and \"user\""})
		return
	}
	st, err := h.room.Join(r.Context(), req.Ticket, req.User)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ticketParam returns the ticket query parameter, or writes the error.
func ticketParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.URL.Query().Get("ticket")
	if !validTicket(id) {
		writeError(w, ErrInvalidTicket)
		return "", false
	}
	return id, true
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	id, ok := ticketParam(w, r)
	if !ok {
		return
	}
	st := h.room.Status(id)
	code := http.StatusOK
	if st.State == StateUnknown {
		code = http.StatusNotFound
	}
	writeJSON(w, code, st)
}

func (h *handler) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.room.State().Stats())
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	s := h.room.State().Stats()
	if s.QueueEnd == 0 && s.GateEnd == 0 {
		// Nothing read yet: either the logs are empty or the room cannot reach
		// them. A load balancer should not send people here until it has read.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no records read yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// watch streams the status of a ticket as server-sent events: one "status"
// event now and one after every change that affects it.
func (h *handler) watch(w http.ResponseWriter, r *http.Request) {
	id, ok := ticketParam(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming is not supported here"})
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no") // do not let a reverse proxy hold the stream back
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	keepalive := time.NewTicker(h.keepalive)
	defer keepalive.Stop()
	var last []byte
	for {
		changed := h.room.State().Changed() // before reading, so no change is missed
		data, err := json.Marshal(h.room.Status(id))
		if err != nil {
			return
		}
		// Many changes do not concern this ticket (someone else joined behind it);
		// send only what differs from the last message.
		if !bytes.Equal(data, last) {
			if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", data); err != nil {
				return
			}
			fl.Flush()
			last = data
		}
		select {
		case <-changed:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
