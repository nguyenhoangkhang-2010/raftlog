// Package waitingroom is a virtual waiting room built on raftlog, for the moment
// everybody tries to register for the same courses at once.
//
// People join a queue and are let in, in order, a few at a time. All of it is
// kept in two raftlog topics: the order of arrival in "queue", the progress of
// the gate in "gate". The process that serves the web page holds no data of its
// own; it rebuilds the room by reading both topics, so it can be restarted or
// replaced, and the room survives the loss of a raftlog node.
package waitingroom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Default topic names.
const (
	DefaultQueueTopic = "queue"
	DefaultGateTopic  = "gate"
)

// Log is the part of the raftlog client the room uses. *client.Client implements
// it; tests can use a fake.
type Log interface {
	CreateTopic(ctx context.Context, topic string, partitions int) error
	Produce(ctx context.Context, topic string, msgs []client.Message, opts ...client.ProduceOption) (client.ProduceResult, error)
	Fetch(ctx context.Context, topic string, partition int, offset uint64, limit int, opts ...client.FetchOption) ([]client.Record, error)
	GroupPosition(ctx context.Context, group, topic string, partition int) (client.GroupPosition, error)
}

var _ Log = (*client.Client)(nil)

// Config configures a Room.
type Config struct {
	QueueTopic string
	GateTopic  string
	// PollWait is how long a read of a log waits for new records before it asks
	// again. It must be shorter than the node's -max-fetch-wait to be effective.
	PollWait time.Duration
	// RetryPause is how long to wait after a failed read before trying again.
	RetryPause time.Duration
	// FetchLimit is the most records read in one request.
	FetchLimit int
	// RequestTimeout bounds one request to the log made for a caller (a join, an
	// admission). Reads run until the room stops.
	RequestTimeout time.Duration
	// Logger receives operational messages; nil discards them.
	Logger *slog.Logger
}

func (c *Config) validate() error {
	switch {
	case c.QueueTopic == "" || c.GateTopic == "" || c.QueueTopic == c.GateTopic:
		return errors.New("waitingroom: the queue and gate topics must be two different names")
	case c.PollWait <= 0:
		return errors.New("waitingroom: PollWait must be positive")
	case c.RetryPause <= 0:
		return errors.New("waitingroom: RetryPause must be positive")
	case c.FetchLimit < 1:
		return errors.New("waitingroom: FetchLimit must be at least 1")
	case c.RequestTimeout <= 0:
		return errors.New("waitingroom: RequestTimeout must be positive")
	}
	return nil
}

// Room is a waiting room. It is safe for concurrent use.
type Room struct {
	log   Log
	cfg   Config
	state *State
	slog  *slog.Logger
}

// New returns a Room that keeps its data in log.
func New(log Log, cfg Config) (*Room, error) {
	if log == nil {
		return nil, errors.New("waitingroom: a log is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l := cfg.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Room{log: log, cfg: cfg, state: NewState(), slog: l}, nil
}

// State returns the room's view of the two logs.
func (r *Room) State() *State { return r.state }

// Setup creates the two topics if they are not there. Both have one partition:
// the order of the queue is the order of one log.
func (r *Room) Setup(ctx context.Context) error {
	for _, topic := range []string{r.cfg.QueueTopic, r.cfg.GateTopic} {
		err := r.log.CreateTopic(ctx, topic, 1)
		if err != nil && !errors.Is(err, client.ErrAlreadyExists) {
			return fmt.Errorf("create topic %q: %w", topic, err)
		}
	}
	return nil
}

// Run keeps the room up to date by reading both logs until ctx ends. It does not
// return an error for a log that is unreachable: it keeps trying.
func (r *Room) Run(ctx context.Context) error {
	done := make(chan struct{}, 2)
	go func() { r.follow(ctx, r.cfg.QueueTopic, r.state.ApplyQueue); done <- struct{}{} }()
	go func() { r.follow(ctx, r.cfg.GateTopic, r.state.ApplyGate); done <- struct{}{} }()
	<-done
	<-done
	return ctx.Err()
}

// follow reads one log from its start, applying every record, until ctx ends.
func (r *Room) follow(ctx context.Context, topic string, apply func(client.Record)) {
	var next uint64
	failing := false
	for ctx.Err() == nil {
		recs, err := r.log.Fetch(ctx, topic, 0, next, r.cfg.FetchLimit, client.WaitUpTo(r.cfg.PollWait))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The topic may not exist yet (Setup has not run), or the node that
			// answered is behind the one we read from before (offset out of range),
			// or the cluster is without a leader. All of these pass.
			if !failing {
				r.slog.Warn("reading the log failed; will retry", "topic", topic, "offset", next, "err", err)
				failing = true
			}
			sleep(ctx, r.cfg.RetryPause)
			continue
		}
		if failing {
			r.slog.Info("reading the log again", "topic", topic, "offset", next)
			failing = false
		}
		for _, rec := range recs {
			apply(rec)
			next = rec.Offset + 1
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// WaitCaughtUp blocks until the room has read both logs up to where they ended
// when it asked. The admission controller calls it before it starts, so that it
// decides on the whole history and not on a part of it.
func (r *Room) WaitCaughtUp(ctx context.Context) error {
	var ends [2]uint64
	for i, topic := range []string{r.cfg.QueueTopic, r.cfg.GateTopic} {
		for {
			pos, err := r.log.GroupPosition(ctx, "waitingroom-probe", topic, 0)
			if err == nil {
				ends[i] = pos.End
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sleep(ctx, r.cfg.RetryPause)
		}
	}
	return r.state.WaitRead(ctx, ends[0], ends[1])
}

// Join puts a ticket in the queue, unless it is there already, and returns its
// status once the room has seen the join. The ticket ID is chosen by the caller
// and joining again with it is harmless: the ticket keeps its first place.
func (r *Room) Join(ctx context.Context, ticket, user string) (Status, error) {
	if !validTicket(ticket) {
		return Status{}, ErrInvalidTicket
	}
	user, err := cleanUser(user)
	if err != nil {
		return Status{}, err
	}
	if st := r.state.Status(ticket); st.State != StateUnknown {
		return st, nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	value, err := encodeJoined(ticket, user)
	if err != nil {
		return Status{}, err
	}
	res, err := r.log.Produce(ctx, r.cfg.QueueTopic, []client.Message{{Key: []byte(ticket), Value: value}}, client.ToPartition(0))
	if err != nil {
		return Status{}, fmt.Errorf("join: %w", err)
	}
	// The answer must include the place, so wait until this process has read its
	// own write back from the log.
	if err := r.state.WaitQueue(ctx, res.Offset); err != nil {
		return Status{}, fmt.Errorf("join: the room did not see the ticket in time: %w", err)
	}
	return r.state.Status(ticket), nil
}

// Status returns what the room knows about a ticket.
func (r *Room) Status(ticket string) Status { return r.state.Status(ticket) }

// Admit lets the next batch of waiting tickets in, by writing a gate event. It
// returns how many tickets that was, 0 if nobody is waiting. The new gate is
// visible in the room's state when Admit returns.
func (r *Room) Admit(ctx context.Context, batch int) (int, error) {
	upTo, count := r.state.NextBoundary(batch)
	if count == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	value, err := encodeAdmitted(upTo, count)
	if err != nil {
		return 0, err
	}
	res, err := r.log.Produce(ctx, r.cfg.GateTopic, []client.Message{{Value: value}}, client.ToPartition(0))
	if err != nil {
		return 0, fmt.Errorf("admit: %w", err)
	}
	if err := r.state.WaitGate(ctx, res.Offset); err != nil {
		return count, fmt.Errorf("admit: the room did not see the gate move in time: %w", err)
	}
	return count, nil
}

// RunController admits batch tickets every interval until ctx ends. Run exactly
// one controller per room: with two, the room is admitted twice as fast, though
// never out of order and never twice (the gate only moves forward).
//
// A controller that restarts first reads the whole history, so that it does not
// write admissions the gate has already passed. A room that is behind could only
// work out a gate that is too low, never too high, so even that would be harmless.
func (r *Room) RunController(ctx context.Context, interval time.Duration, batch int) error {
	if interval <= 0 || batch < 1 {
		return errors.New("waitingroom: the admission interval must be positive and the batch at least 1")
	}
	if err := r.WaitCaughtUp(ctx); err != nil {
		return err
	}
	r.slog.Info("admission controller running", "interval", interval, "batch", batch)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		n, err := r.Admit(ctx, batch)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			r.slog.Warn("admission failed; will try again at the next tick", "err", err)
		case n > 0:
			s := r.state.Stats()
			r.slog.Info("admitted", "count", n, "admitted", s.Admitted, "waiting", s.Waiting)
		}
	}
}
