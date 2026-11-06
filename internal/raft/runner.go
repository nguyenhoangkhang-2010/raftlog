package raft

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

var (
	// ErrLeadershipLost is returned by Propose when this node stopped being the
	// leader before the entry was known to be committed. The entry may still be
	// committed by the next leader, so the outcome is unknown.
	ErrLeadershipLost = errors.New("raft: leadership lost before the entry was known to be committed; it may still commit")
	// ErrEntryReplaced is returned by Propose when a different entry took the
	// place of the proposed one in the log. The proposal definitely did not commit.
	ErrEntryReplaced = errors.New("raft: the proposed entry was replaced by another leader's entry")
	// ErrStopped is returned by Propose when the runner stops while waiting.
	ErrStopped = errors.New("raft: runner stopped")
)

// Transport sends one message to the node named in m.To. Delivery is best
// effort: Raft copes with lost, delayed and duplicated messages, so Send may
// fail without anyone retrying.
type Transport interface {
	Send(ctx context.Context, m Message) error
}

// RunnerOption customizes a Runner.
type RunnerOption func(*Runner)

// WithApplier sets the function that applies committed commands to the state
// machine. It is called from a single goroutine, in log order, once per entry
// that carries a command (no-op entries are skipped). After a restart the log
// is replayed from index 1, so a state machine with its own durable state must
// ignore entries it has already applied.
func WithApplier(apply func(Entry)) RunnerOption {
	return func(r *Runner) { r.applier = apply }
}

// peerQueueSize bounds the messages waiting for one slow or dead peer. When it
// is full the oldest message is dropped: a newer heartbeat is worth more than
// an old one.
const peerQueueSize = 64

// waiter is a Propose call waiting for its entry to be applied.
type waiter struct {
	term uint64
	done chan error
}

// Runner drives a Node with a real clock and a real Transport.
//
// A goroutine calls Tick every tick interval, incoming messages arrive through
// Receive, and one goroutine per peer sends the outgoing messages so that a
// slow or dead peer never delays the others or the protocol itself. A further
// goroutine applies committed entries, so a slow state machine never blocks
// the protocol either.
type Runner struct {
	mu   sync.Mutex // guards everything below up to "stop"
	cond *sync.Cond // signalled when applyQueue gets entries or the runner halts
	node *Node

	applyQueue []Entry
	waiters    map[uint64]*waiter // by log index
	halted     bool

	applier     func(Entry)
	tr          Transport
	tick        time.Duration
	sendTimeout time.Duration
	logger      *slog.Logger
	queues      map[NodeID]chan Message

	stop    chan struct{}
	stopped sync.Once
	started bool
	wg      sync.WaitGroup
}

// NewRunner creates a runner. tickInterval is the length of one logical tick;
// together with cfg.ElectionTicks and cfg.HeartbeatTicks it fixes the real
// election timeout and heartbeat period.
func NewRunner(cfg Config, tr Transport, tickInterval time.Duration, opts ...RunnerOption) (*Runner, error) {
	if tr == nil {
		return nil, errors.New("raft: Transport is required")
	}
	if tickInterval <= 0 {
		return nil, errors.New("raft: tick interval must be positive")
	}
	node, err := New(cfg)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		node:        node,
		waiters:     make(map[uint64]*waiter),
		tr:          tr,
		tick:        tickInterval,
		sendTimeout: tickInterval * time.Duration(cfg.ElectionTicks), // never outlive an election timeout
		logger:      cfg.Logger,
		queues:      make(map[NodeID]chan Message),
		stop:        make(chan struct{}),
	}
	r.cond = sync.NewCond(&r.mu)
	if r.logger == nil {
		r.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, opt := range opts {
		opt(r)
	}
	for _, p := range node.peers {
		r.queues[p] = make(chan Message, peerQueueSize)
	}
	return r, nil
}

// Start begins ticking, sending and applying. It does nothing if already started.
func (r *Runner) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	for id, q := range r.queues {
		r.wg.Add(1)
		go r.sender(id, q)
	}
	r.wg.Add(2)
	go r.loop()
	go r.applyLoop()
}

// Stop stops the goroutines and waits for them. Entries already committed are
// still applied first. It is safe to call twice.
func (r *Runner) Stop() {
	r.stopped.Do(func() {
		close(r.stop)
		r.mu.Lock()
		r.halted = true
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	r.wg.Wait()
}

// Status returns a snapshot of the node.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node.Status()
}

// Receive hands a message from another node to the protocol. Transports call
// it for every incoming message.
func (r *Runner) Receive(m Message) error {
	r.mu.Lock()
	err := r.node.Step(m)
	msgs := r.flushLocked()
	r.mu.Unlock()

	r.dispatch(msgs)
	return err
}

// Propose submits a command and waits until it is committed and applied, the
// leadership changes, or ctx ends. It returns the log index of the entry.
//
// On a node that is not the leader it fails at once with a *NotLeaderError.
// If it fails after the entry was appended (ErrLeadershipLost, ctx errors,
// ErrStopped) the entry may still commit later; callers that retry must
// tolerate duplicates.
func (r *Runner) Propose(ctx context.Context, data []byte) (uint64, error) {
	r.mu.Lock()
	index, term, err := r.node.Propose(data)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	w := &waiter{term: term, done: make(chan error, 1)}
	r.waiters[index] = w
	msgs := r.flushLocked()
	r.mu.Unlock()

	r.dispatch(msgs)

	select {
	case err := <-w.done:
		return index, err
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.waiters, index)
		r.mu.Unlock()
		return index, ctx.Err()
	case <-r.stop:
		return index, ErrStopped
	}
}

// flushLocked collects what the node produced after a step: messages to send
// and newly committed entries to apply. The caller holds r.mu; entries are
// queued under the lock so that they are applied in log order.
func (r *Runner) flushLocked() []Message {
	msgs := r.node.Messages()

	entries, err := r.node.Committed()
	if err != nil {
		r.logger.Error("raft: cannot read committed entries", "err", err)
	}
	if len(entries) > 0 {
		r.applyQueue = append(r.applyQueue, entries...)
		r.cond.Signal()
	}

	// Proposals of a leadership that has ended will not be answered by this
	// node's own commit any more. Entries already handed to the apply queue are
	// excluded: those are resolved precisely when they are applied.
	st := r.node.Status()
	for index, w := range r.waiters {
		if index > r.node.lastApplied && (st.Role != Leader || st.Term != w.term) {
			w.done <- ErrLeadershipLost
			delete(r.waiters, index)
		}
	}
	return msgs
}

func (r *Runner) loop() {
	defer r.wg.Done()
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.mu.Lock()
			err := r.node.Tick()
			msgs := r.flushLocked()
			r.mu.Unlock()

			if err != nil {
				r.logger.Error("raft tick failed", "err", err)
			}
			r.dispatch(msgs)
		}
	}
}

// applyLoop applies committed entries, one at a time, in log order.
func (r *Runner) applyLoop() {
	defer r.wg.Done()
	for {
		r.mu.Lock()
		for len(r.applyQueue) == 0 && !r.halted {
			r.cond.Wait()
		}
		if len(r.applyQueue) == 0 {
			r.mu.Unlock()
			return // halted and nothing left to apply
		}
		batch := r.applyQueue
		r.applyQueue = nil
		r.mu.Unlock()

		for _, e := range batch {
			if e.Type == EntryNormal && r.applier != nil {
				r.applier(e)
			}
			r.mu.Lock()
			if w, ok := r.waiters[e.Index]; ok {
				delete(r.waiters, e.Index)
				if w.term == e.Term {
					w.done <- nil
				} else {
					w.done <- ErrEntryReplaced
				}
			}
			r.mu.Unlock()
		}
	}
}

// dispatch queues messages for the per-peer senders without blocking.
func (r *Runner) dispatch(msgs []Message) {
	for _, m := range msgs {
		q, ok := r.queues[m.To]
		if !ok {
			continue
		}
		select {
		case q <- m:
		default:
			// Full: drop the oldest message and try once more.
			select {
			case <-q:
			default:
			}
			select {
			case q <- m:
			default:
			}
		}
	}
}

func (r *Runner) sender(to NodeID, q chan Message) {
	defer r.wg.Done()
	for {
		select {
		case <-r.stop:
			return
		case m := <-q:
			ctx, cancel := context.WithTimeout(context.Background(), r.sendTimeout)
			err := r.tr.Send(ctx, m)
			cancel()
			if err != nil {
				r.logger.Debug("raft send failed", "to", to, "type", m.Type.String(), "err", err)
			}
		}
	}
}
