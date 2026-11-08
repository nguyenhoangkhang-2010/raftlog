package console

import (
	"context"
	_ "embed" // the page is part of the binary
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

//go:embed web/index.html
var indexHTML []byte

// Controller starts and kills node processes. *Supervisor implements it.
type Controller interface {
	Start(id int) error
	Kill(id int) error
}

// Lister reads the contents of the cluster. *client.Client implements it.
type Lister interface {
	Topics(ctx context.Context) ([]client.TopicInfo, error)
	Groups(ctx context.Context) ([]client.GroupInfo, error)
}

// ServerOptions wires the console together.
type ServerOptions struct {
	Observer *Observer
	// Control is nil for a console that only watches a cluster it does not run.
	Control Controller
	Load    *Load
	Lister  Lister
	// VerifyPatience is how long a verification waits for a lagging node to show a
	// message it was told was stored.
	VerifyPatience time.Duration
}

// TopicView is a topic as the console shows it.
type TopicView struct {
	Name       string `json:"name"`
	Partitions int    `json:"partitions"`
	Messages   uint64 `json:"messages"`
}

// GroupView is a consumer group as the console shows it.
type GroupView struct {
	Name string `json:"name"`
	Lag  uint64 `json:"lag"`
}

// State is everything the page shows, in one answer.
type State struct {
	Now         time.Time   `json:"now"`
	Control     bool        `json:"control"`
	Summary     Summary     `json:"summary"`
	Nodes       []NodeView  `json:"nodes"`
	Events      []Event     `json:"events"`
	Load        LoadStats   `json:"load"`
	Topics      []TopicView `json:"topics"`
	Groups      []GroupView `json:"groups"`
	TopicsError string      `json:"topics_error,omitempty"`
}

type server struct {
	opts ServerOptions

	mu          sync.Mutex
	topics      []TopicView
	groups      []GroupView
	topicsErr   string
	topicsFetch time.Time
}

// requestMarker must be on every request that changes something. A web page on
// another origin cannot add a custom header without the console's permission, so
// this keeps such a page from stopping a node through the browser of someone who
// has the console open.
const requestMarker = "console"

// NewServer returns the HTTP handler of the console.
func NewServer(opts ServerOptions) (http.Handler, error) {
	if opts.Observer == nil || opts.Load == nil || opts.Lister == nil {
		return nil, errors.New("console: the observer, the load generator and a lister are required")
	}
	if opts.VerifyPatience <= 0 {
		return nil, errors.New("console: VerifyPatience must be positive")
	}
	s := &server{opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/nodes/{id}/kill", s.guarded(s.kill))
	mux.HandleFunc("POST /api/nodes/{id}/start", s.guarded(s.start))
	mux.HandleFunc("POST /api/load", s.guarded(s.loadStart))
	mux.HandleFunc("POST /api/load/stop", s.guarded(s.loadStop))
	mux.HandleFunc("POST /api/load/reset", s.guarded(s.loadReset))
	mux.HandleFunc("POST /api/verify", s.guarded(s.verify))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// guarded refuses a request that does not carry the marker.
func (s *server) guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Requested-With") != requestMarker {
			writeErr(w, http.StatusForbidden, errors.New("this request must come from the console page"))
			return
		}
		h(w, r)
	}
}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

// refreshTopics reads the topics and groups, at most once a second.
func (s *server) refreshTopics(ctx context.Context) {
	s.mu.Lock()
	if time.Since(s.topicsFetch) < time.Second {
		s.mu.Unlock()
		return
	}
	s.topicsFetch = time.Now()
	s.mu.Unlock()

	qctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	topics, err := s.opts.Lister.Topics(qctx)
	var groups []client.GroupInfo
	if err == nil {
		groups, err = s.opts.Lister.Groups(qctx)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.topicsErr = err.Error() // keep showing the last good answer
		return
	}
	s.topicsErr = ""
	s.topics = s.topics[:0]
	for _, t := range topics {
		var total uint64
		for _, e := range t.Ends {
			total += e
		}
		s.topics = append(s.topics, TopicView{Name: t.Name, Partitions: t.Partitions(), Messages: total})
	}
	s.groups = s.groups[:0]
	for _, g := range groups {
		var lag uint64
		for _, p := range g.Partitions {
			lag += p.Lag
		}
		s.groups = append(s.groups, GroupView{Name: g.Name, Lag: lag})
	}
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	s.refreshTopics(r.Context())
	nodes, sum, events := s.opts.Observer.Snapshot()
	s.mu.Lock()
	st := State{
		Now: time.Now(), Control: s.opts.Control != nil, Summary: sum, Nodes: nodes, Events: events,
		Load: s.opts.Load.Stats(), Topics: append([]TopicView{}, s.topics...), Groups: append([]GroupView{}, s.groups...),
		TopicsError: s.topicsErr,
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

func (s *server) nodeID(w http.ResponseWriter, r *http.Request) (int, bool) {
	if s.opts.Control == nil {
		writeErr(w, http.StatusForbidden, errors.New("this console only watches the cluster; it does not run its nodes"))
		return 0, false
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeErr(w, http.StatusBadRequest, errors.New("the node ID must be a positive number"))
		return 0, false
	}
	nodes, _, _ := s.opts.Observer.Snapshot()
	for _, n := range nodes {
		if n.ID == id {
			return id, true
		}
	}
	// Before the first poll nothing is known: ask the observer's own list instead.
	for _, n := range s.opts.Observer.nodes {
		if n.ID == id {
			return id, true
		}
	}
	writeErr(w, http.StatusNotFound, fmt.Errorf("there is no node %d", id))
	return 0, false
}

func (s *server) kill(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	if err := s.opts.Control.Kill(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.opts.Observer.Record("action", fmt.Sprintf("you killed node %d", id))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	if err := s.opts.Control.Start(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.opts.Observer.Record("action", fmt.Sprintf("you started node %d", id))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) loadStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rate int `json:"rate"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New(`the body must be {"rate": messages per second}`))
		return
	}
	if err := s.opts.Load.Start(body.Rate); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.opts.Observer.Record("action", fmt.Sprintf("you started writing %d messages per second", body.Rate))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) loadStop(w http.ResponseWriter, _ *http.Request) {
	s.opts.Load.Stop()
	s.opts.Observer.Record("action", "you stopped the writes")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) loadReset(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Load.Stats().Running {
		writeErr(w, http.StatusConflict, errors.New("stop the writes before resetting the counters"))
		return
	}
	s.opts.Load.Reset()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) verify(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	v, err := s.opts.Load.Verify(ctx, s.opts.VerifyPatience)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.opts.Observer.Record("action", fmt.Sprintf("verified %d acknowledged writes: %d missing", v.Acked, v.Missing))
	writeJSON(w, http.StatusOK, v)
}
