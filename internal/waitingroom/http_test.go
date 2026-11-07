package waitingroom

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

func startWeb(t *testing.T, keepalive time.Duration) (*Room, *httptest.Server) {
	t.Helper()
	room := startRoom(t, newClient(t, startBroker(t)), testConfig())
	h, err := NewHandler(room, HandlerOptions{Keepalive: keepalive})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return room, ts
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestTheHandlerNeedsAKeepalive(t *testing.T) {
	r, err := New(newClient(t, startBroker(t)), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(r, HandlerOptions{}); err == nil {
		t.Fatal("accepted a zero keepalive")
	}
}

func TestThePageIsServed(t *testing.T) {
	_, ts := startWeb(t, time.Minute)
	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"/api/join", "/api/watch", "EventSource"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not use %s", want)
		}
	}
	if resp, _ := get(t, ts.URL+"/nowhere"); resp.StatusCode != 404 {
		t.Errorf("GET /nowhere: %d, want 404", resp.StatusCode)
	}
}

func TestJoinAndStatusOverHTTP(t *testing.T) {
	_, ts := startWeb(t, time.Minute)

	for i := 0; i < 3; i++ {
		resp, body := post(t, ts.URL+"/api/join", fmt.Sprintf(`{"ticket":"t%d","user":"student %d"}`, i, i))
		if resp.StatusCode != 200 {
			t.Fatalf("join %d: %d %s", i, resp.StatusCode, body)
		}
		var st Status
		if err := json.Unmarshal([]byte(body), &st); err != nil || st.Position != i+1 || st.State != StateWaiting {
			t.Fatalf("join %d: %s (%v)", i, body, err)
		}
	}
	resp, body := get(t, ts.URL+"/api/status?ticket=t1")
	var st Status
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &st) != nil || st.Position != 2 || st.Total != 3 {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, ts.URL+"/api/status?ticket=never"); resp.StatusCode != 404 {
		t.Errorf("status of an unknown ticket: %d, want 404", resp.StatusCode)
	}
	resp, body = get(t, ts.URL+"/api/stats")
	var stats Stats
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &stats) != nil || stats.Total != 3 || stats.Waiting != 3 {
		t.Fatalf("stats: %d %s", resp.StatusCode, body)
	}
}

func TestBadRequestsAreRefusedWithAClearError(t *testing.T) {
	_, ts := startWeb(t, time.Minute)
	cases := map[string]string{
		"not json":       `nope`,
		"unknown field":  `{"ticket":"a","extra":1}`,
		"no ticket":      `{"user":"x"}`,
		"bad ticket":     `{"ticket":"a b"}`,
		"bad user":       `{"ticket":"a","user":"x\ny"}`,
		"oversized body": `{"ticket":"a","user":"` + strings.Repeat("x", 10_000) + `"}`,
	}
	for name, body := range cases {
		resp, got := post(t, ts.URL+"/api/join", body)
		if resp.StatusCode != 400 || !strings.Contains(got, `"error"`) {
			t.Errorf("%s: %d %s, want 400 with an error", name, resp.StatusCode, got)
		}
	}
	if resp, _ := get(t, ts.URL+"/api/status?ticket=bad%20one"); resp.StatusCode != 400 {
		t.Errorf("status with a bad ticket: %d, want 400", resp.StatusCode)
	}
	if resp, _ := get(t, ts.URL+"/api/watch"); resp.StatusCode != 400 {
		t.Errorf("watch without a ticket: %d, want 400", resp.StatusCode)
	}
	if resp, _ := get(t, ts.URL+"/api/join"); resp.StatusCode != 405 {
		t.Errorf("GET /api/join: %d, want 405", resp.StatusCode)
	}
}

func TestHealthWaitsForTheFirstRecords(t *testing.T) {
	room, ts := startWeb(t, time.Minute)
	if resp, _ := get(t, ts.URL+"/healthz"); resp.StatusCode != 503 {
		t.Fatalf("healthz before any record: %d, want 503", resp.StatusCode)
	}
	if _, err := room.Join(bg(t), "a", ""); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, ts.URL+"/healthz"); resp.StatusCode != 200 {
		t.Fatalf("healthz after a record was read: %d, want 200", resp.StatusCode)
	}
}

// sse reads server-sent events from a response.
type sse struct {
	events chan string // "name data" per event, comments as ": text"
	cancel context.CancelFunc
}

func openSSE(t *testing.T, url string) *sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		t.Fatalf("watch: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sse{events: make(chan string, 100), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				s.events <- line
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				s.events <- name + " " + strings.TrimPrefix(line, "data: ")
			}
		}
		close(s.events)
	}()
	t.Cleanup(cancel)
	return s
}

func (s *sse) next(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case e, ok := <-s.events:
		if !ok {
			t.Fatal("the stream ended")
		}
		return e
	case <-time.After(d):
		t.Fatalf("no event within %v", d)
		return ""
	}
}

func (s *sse) status(t *testing.T) Status {
	t.Helper()
	for {
		e := s.next(t, 5*time.Second)
		if strings.HasPrefix(e, "status ") {
			var st Status
			if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "status ")), &st); err != nil {
				t.Fatal(err)
			}
			return st
		}
	}
}

func TestWatchPushesChangesAsTheyHappen(t *testing.T) {
	room, ts := startWeb(t, time.Minute)
	for i := 0; i < 4; i++ {
		if _, err := room.Join(bg(t), fmt.Sprintf("t%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	s := openSSE(t, ts.URL+"/api/watch?ticket=t2")
	if st := s.status(t); st.State != StateWaiting || st.Position != 3 {
		t.Fatalf("first event: %+v, want waiting at position 3", st)
	}

	if _, err := room.Admit(bg(t), 1); err != nil {
		t.Fatal(err)
	}
	if st := s.status(t); st.Position != 2 {
		t.Fatalf("after one admission: %+v, want position 2", st)
	}
	if _, err := room.Admit(bg(t), 2); err != nil {
		t.Fatal(err)
	}
	if st := s.status(t); st.State != StateAdmitted {
		t.Fatalf("after three admissions: %+v, want admitted", st)
	}
}

func TestWatchStaysQuietWhenNothingAboutTheTicketChanged(t *testing.T) {
	room, ts := startWeb(t, time.Hour)
	if _, err := room.Join(bg(t), "me", ""); err != nil {
		t.Fatal(err)
	}
	s := openSSE(t, ts.URL+"/api/watch?ticket=me")
	if st := s.status(t); st.Total != 1 {
		t.Fatalf("first event: %+v", st)
	}
	// Someone joins behind me. The totals change, so I hear about it once...
	if _, err := room.Join(bg(t), "behind", ""); err != nil {
		t.Fatal(err)
	}
	if st := s.status(t); st.Total != 2 || st.Position != 1 {
		t.Fatalf("after a join behind me: %+v", st)
	}
	// ...but a repeated join event (a retry that reached the log twice) changes
	// nothing for me, and so no message may follow.
	dup, _ := encodeJoined("behind", "")
	res, err := room.log.Produce(bg(t), room.cfg.QueueTopic, []client.Message{{Value: dup}}, client.ToPartition(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := room.State().WaitQueue(bg(t), res.Offset); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-s.events:
		t.Fatalf("an event for a change that did not happen: %q", e)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWatchSendsKeepalives(t *testing.T) {
	_, ts := startWeb(t, 30*time.Millisecond)
	s := openSSE(t, ts.URL+"/api/watch?ticket=idle")
	if e := s.next(t, 5*time.Second); !strings.HasPrefix(e, "status ") {
		t.Fatalf("first event: %q", e)
	}
	if e := s.next(t, 5*time.Second); !strings.HasPrefix(e, ": keepalive") {
		t.Fatalf("second event: %q, want a keepalive comment", e)
	}
}

func TestWatchEndsWhenTheClientLeaves(t *testing.T) {
	_, ts := startWeb(t, time.Minute)
	s := openSSE(t, ts.URL+"/api/watch?ticket=gone")
	s.status(t)
	s.cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream did not end")
		}
	}
}

func TestErrorsMapToHTTPStatuses(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrInvalidTicket, 400},
		{fmt.Errorf("x: %w", ErrInvalidUser), 400},
		{fmt.Errorf("join: %w", client.ErrUnavailable), 503},
		{fmt.Errorf("join: %w", client.ErrClosed), 503},
		{fmt.Errorf("join: %w", context.DeadlineExceeded), 504},
		{fmt.Errorf("something else"), 500},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, c.err)
		if rec.Code != c.want {
			t.Errorf("%v: %d, want %d", c.err, rec.Code, c.want)
		}
	}
}
