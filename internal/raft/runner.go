package raft

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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

// StateMachine is the application that the replicated log feeds. All three
// methods are called from one goroutine, one at a time, in log order.
type StateMachine interface {
	// Apply executes one committed command (no-op entries are not passed) and
	// returns its outcome. The outcome reaches the caller of ProposeResult on
	// the node that proposed the command; on other nodes it is dropped. It
	// must depend only on the command and on the state built by earlier ones,
	// or the nodes would drift apart.
	Apply(Entry) any
	// Snapshot returns the complete state after the last entry passed to Apply.
	Snapshot() ([]byte, error)
	// Restore replaces the whole state with a snapshot taken by Snapshot, on
	// this node or on another one.
	Restore([]byte) error
}

// BatchStateMachine is a StateMachine that can apply several consecutive entries in
// one call, which lets it make them durable together instead of one at a time. The
// runner uses ApplyBatch when the state machine has it.
type BatchStateMachine interface {
	StateMachine
	// ApplyBatch applies the entries, which are consecutive committed commands in log
	// order (no-op entries are not passed), and returns the outcome of each, in the same
	// order. The state afterwards, and each outcome, must be what calling Apply on each
	// entry in turn would have given.
	ApplyBatch([]Entry) []any
}

// RunnerOption customizes a Runner.
type RunnerOption func(*Runner)

// WithApplier sets a function that applies committed commands to a state
// machine that does not support snapshots. It is called from a single
// goroutine, in log order, once per entry that carries a command (no-op entries
// are skipped). After a restart the log is replayed from index 1, so a state
// machine with its own durable state must ignore entries it has already applied.
func WithApplier(apply func(Entry)) RunnerOption {
	return func(r *Runner) { r.applier = apply }
}

// WithStateMachine sets the state machine that the log feeds. Unlike
// WithApplier it supports snapshots, so the log can be compacted.
func WithStateMachine(sm StateMachine) RunnerOption {
	return func(r *Runner) { r.sm = sm }
}

// WithSnapshotEvery makes the runner snapshot the state machine, and cut the
// log, after every n applied entries. It needs WithStateMachine. Zero, the
// default, disables snapshots on this node (it can still receive them).
func WithSnapshotEvery(n uint64) RunnerOption {
	return func(r *Runner) { r.snapshotEvery = n }
}

// peerQueueSize bounds the messages waiting for one slow or dead peer. When it
// is full the oldest message is dropped: a newer heartbeat is worth more than
// an old one.
const peerQueueSize = 64

// waitResult is how a Propose call ends: the state machine's outcome for the
// command, or an error.
type waitResult struct {
	value any
	err   error
}

// waiter is a Propose call waiting for its entry to be applied.
type waiter struct {
	term uint64
	done chan waitResult
}

// A proposal batch ends when it holds this many commands or this many bytes of them.
// Nothing waits to fill a batch: the proposer takes what is queued when it is free.
const (
	proposeBatchMaxEntries = 256
	proposeBatchMaxBytes   = 4 << 20
	proposeQueueSize       = 1024
)

// proposal is one Propose call handed to the proposer goroutine.
type proposal struct {
	data []byte
	w    *waiter         // registered under the entry's index before the entry can apply
	ack  chan proposeAck // answered exactly once, with the index or the reason it failed
}

type proposeAck struct {
	index uint64
	err   error
}

// applyItem is one unit of work for the apply goroutine: either a snapshot the
// state machine must load, or a committed entry to apply.
type applyItem struct {
	restore *Snapshot
	entry   Entry
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
	cond *sync.Cond // signalled when applyQueue gets items or the runner halts
	node *Node

	applyQueue []applyItem
	waiters    map[uint64]*waiter // by log index
	halted     bool

	proposals chan *proposal // from Propose calls to the proposer goroutine

	applier       func(Entry)
	sm            StateMachine
	snapshotEvery uint64
	snapIndex     uint64 // index of the latest snapshot; used by the apply goroutine

	tr          Transport
	tick        time.Duration
	sendTimeout time.Duration
	logger      *slog.Logger
	queues      map[NodeID]chan Message

	// Counters for observation only (see Observe).
	sendFailures       map[NodeID]*atomic.Uint64 // fixed at creation; the counters change
	dropped            atomic.Uint64             // messages thrown away because a peer's queue was full
	snapshotsTaken     atomic.Uint64
	snapshotsInstalled atomic.Uint64

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
		proposals:   make(chan *proposal, proposeQueueSize),
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
	if r.snapshotEvery > 0 && r.sm == nil {
		return nil, errors.New("raft: WithSnapshotEvery needs WithStateMachine")
	}
	r.sendFailures = make(map[NodeID]*atomic.Uint64, len(node.peers))
	for _, p := range node.peers {
		r.queues[p] = make(chan Message, peerQueueSize)
		r.sendFailures[p] = new(atomic.Uint64)
	}

	// A snapshot found at startup is the first thing the state machine loads.
	r.snapIndex = node.snapshot.Index
	r.mu.Lock()
	r.flushLocked()
	r.mu.Unlock()
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
	r.wg.Add(3)
	go r.loop()
	go r.applyLoop()
	go r.proposeLoop()
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

// Observation is everything worth looking at on a running node, read at one
// moment. None of it influences the protocol.
type Observation struct {
	Status Status
	Stats  Stats
	// ApplyBacklog is how many committed items wait for the state machine.
	ApplyBacklog int
	// SnapshotsTaken and SnapshotsInstalled count snapshots this node made itself
	// and snapshots it received from a leader.
	SnapshotsTaken, SnapshotsInstalled uint64
	// MessagesDropped counts messages thrown away because the queue to a peer was
	// full; Raft recovers from them, a rising number means a peer that cannot keep up.
	MessagesDropped uint64
	// SendFailures counts, per peer, messages the transport failed to deliver.
	SendFailures map[NodeID]uint64
}

// Observe returns the state and the counters of the node.
func (r *Runner) Observe() Observation {
	r.mu.Lock()
	o := Observation{
		Status:       r.node.Status(),
		Stats:        r.node.Stats(),
		ApplyBacklog: len(r.applyQueue),
	}
	r.mu.Unlock()
	o.SnapshotsTaken = r.snapshotsTaken.Load()
	o.SnapshotsInstalled = r.snapshotsInstalled.Load()
	o.MessagesDropped = r.dropped.Load()
	o.SendFailures = make(map[NodeID]uint64, len(r.sendFailures))
	for p, c := range r.sendFailures {
		o.SendFailures[p] = c.Load()
	}
	return o
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
	index, _, err := r.ProposeResult(ctx, data)
	return index, err
}

// ProposeResult is Propose that also returns what the state machine's Apply
// returned for the command (nil if the state machine is an applier function).
//
// Proposals that arrive while the node is busy making an earlier one durable are
// appended to the log together, with one fsync (see proposeLoop). A caller never waits
// for others to arrive: with nothing queued, its command goes in at once.
func (r *Runner) ProposeResult(ctx context.Context, data []byte) (uint64, any, error) {
	p := &proposal{
		data: data,
		w:    &waiter{done: make(chan waitResult, 1)},
		ack:  make(chan proposeAck, 1),
	}
	select {
	case r.proposals <- p:
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-r.stop:
		return 0, nil, ErrStopped
	}

	// Once queued, the command may be proposed even if ctx ends first, like a proposal
	// whose caller gives up after it was appended: it may still commit.
	var index uint64
	select {
	case a := <-p.ack:
		if a.err != nil {
			return 0, nil, a.err
		}
		index = a.index
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-r.stop:
		return 0, nil, ErrStopped
	}

	select {
	case res := <-p.w.done:
		return index, res.value, res.err
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.waiters, index)
		r.mu.Unlock()
		return index, nil, ctx.Err()
	case <-r.stop:
		return index, nil, ErrStopped
	}
}

// proposeLoop appends the queued proposals to the log. It takes the first one that is
// waiting, then every other one that is already there (up to a limit), and makes them
// durable and sends them to the followers together: one fsync and one round of messages
// for the whole group. While it is busy with one group, the next one builds up, so the
// more load there is, the bigger the groups, and with no load a group is a single
// proposal and nothing is delayed.
func (r *Runner) proposeLoop() {
	defer r.wg.Done()
	for {
		var batch []*proposal
		select {
		case <-r.stop:
			return
		case p := <-r.proposals:
			batch = append(batch, p)
		}
		size := len(batch[0].data)
	drain:
		for len(batch) < proposeBatchMaxEntries && size < proposeBatchMaxBytes {
			select {
			case p := <-r.proposals:
				batch = append(batch, p)
				size += len(p.data)
			default:
				break drain
			}
		}
		r.propose(batch)
	}
}

// propose appends one group of proposals as consecutive entries and answers each.
func (r *Runner) propose(batch []*proposal) {
	data := make([][]byte, len(batch))
	for i, p := range batch {
		data[i] = p.data
	}
	r.mu.Lock()
	first, term, err := r.node.ProposeBatch(data)
	if err != nil {
		r.mu.Unlock()
		for _, p := range batch {
			p.ack <- proposeAck{err: err}
		}
		return
	}
	// The waiters are registered before the entries can be applied, and with the term
	// they were proposed in, which is what flushLocked compares against.
	for i, p := range batch {
		p.w.term = term
		r.waiters[first+uint64(i)] = p.w
	}
	msgs := r.flushLocked()
	r.mu.Unlock()

	for i, p := range batch {
		p.ack <- proposeAck{index: first + uint64(i)}
	}
	r.dispatch(msgs)
}

// flushLocked collects what the node produced after a step: messages to send,
// and a snapshot to load and newly committed entries to apply. The caller holds
// r.mu; the work is queued under the lock so that it is applied in order.
func (r *Runner) flushLocked() []Message {
	msgs := r.node.Messages()

	// A snapshot always comes first: it replaces the state that the entries
	// after it build on.
	if s := r.node.TakeRestore(); s != nil {
		r.applyQueue = append(r.applyQueue, applyItem{restore: s})
		r.cond.Signal()
	}
	entries, err := r.node.Committed()
	if err != nil {
		r.logger.Error("raft: cannot read committed entries", "err", err)
	}
	for _, e := range entries {
		r.applyQueue = append(r.applyQueue, applyItem{entry: e})
	}
	if len(entries) > 0 {
		r.cond.Signal()
	}

	// Proposals of a leadership that has ended will not be answered by this
	// node's own commit any more. Entries already handed to the apply queue are
	// excluded: those are resolved precisely when they are applied.
	st := r.node.Status()
	for index, w := range r.waiters {
		if index > r.node.lastApplied && (st.Role != Leader || st.Term != w.term) {
			w.done <- waitResult{err: ErrLeadershipLost}
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

// applyLoop feeds the state machine, one item at a time, in log order.
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

		for i := 0; i < len(batch); {
			if batch[i].restore != nil {
				if !r.restore(batch[i].restore) {
					return
				}
				i++
				continue
			}
			// The entries up to the next snapshot to load go to the state machine together.
			entries := make([]Entry, 0, len(batch)-i)
			for i < len(batch) && batch[i].restore == nil {
				entries = append(entries, batch[i].entry)
				i++
			}
			r.applyEntries(entries)
		}
	}
}

// applyEntries gives consecutive committed entries to the state machine, answers the
// callers waiting for them, and takes a snapshot when one is due. A state machine that
// can apply several entries at once (BatchStateMachine) gets them in groups, which lets
// it make them durable together; otherwise they go one at a time.
//
// A group never runs past the entry at which a snapshot falls due, so snapshots are
// taken at the same indexes as when the entries are applied one by one.
func (r *Runner) applyEntries(entries []Entry) {
	bsm, batched := r.sm.(BatchStateMachine)
	for len(entries) > 0 {
		n := 1
		if batched {
			n = r.groupSize(entries)
		}
		group := entries[:n]
		entries = entries[n:]

		results := make([]any, len(group))
		switch {
		case batched:
			var normal []Entry
			for _, e := range group {
				if e.Type == EntryNormal {
					normal = append(normal, e)
				}
			}
			if len(normal) > 0 {
				out := bsm.ApplyBatch(normal)
				if len(out) != len(normal) {
					r.logger.Error("raft: the state machine answered a batch with the wrong number of outcomes", "entries", len(normal), "outcomes", len(out))
				}
				k := 0
				for i, e := range group {
					if e.Type == EntryNormal {
						if k < len(out) {
							results[i] = out[k]
						}
						k++
					}
				}
			}
		default:
			e := group[0]
			if e.Type == EntryNormal {
				switch {
				case r.sm != nil:
					results[0] = r.sm.Apply(e)
				case r.applier != nil:
					r.applier(e)
				}
			}
		}

		r.mu.Lock()
		for i, e := range group {
			if w, ok := r.waiters[e.Index]; ok {
				delete(r.waiters, e.Index)
				if w.term == e.Term {
					w.done <- waitResult{value: results[i]}
				} else {
					w.done <- waitResult{err: ErrEntryReplaced}
				}
			}
		}
		r.mu.Unlock()

		r.maybeSnapshot(group[len(group)-1].Index)
	}
}

// groupSize is how many of the entries to apply together: at most proposeBatchMaxEntries,
// and not past the entry at which the next snapshot falls due.
func (r *Runner) groupSize(entries []Entry) int {
	n := min(len(entries), proposeBatchMaxEntries)
	if r.snapshotEvery > 0 {
		due := r.snapIndex + r.snapshotEvery
		for i := 0; i < n; i++ {
			if entries[i].Index >= due {
				return i + 1
			}
		}
	}
	return n
}

// restore loads a snapshot into the state machine. It reports false if that
// failed: the state machine is then out of step with the log, and carrying on
// would silently produce wrong results, so the apply loop stops.
func (r *Runner) restore(s *Snapshot) bool {
	r.snapIndex = s.Index
	r.snapshotsInstalled.Add(1)
	if r.sm == nil {
		r.logger.Error("raft: received a snapshot but there is no state machine to load it into", "index", s.Index)
		return true
	}
	if err := r.sm.Restore(s.Data); err != nil {
		r.logger.Error("raft: the state machine failed to load a snapshot; no more entries will be applied", "index", s.Index, "err", err)
		return false
	}
	return true
}

// maybeSnapshot snapshots the state machine once enough entries were applied
// since the previous snapshot, and lets the node cut its log.
func (r *Runner) maybeSnapshot(applied uint64) {
	if r.sm == nil || r.snapshotEvery == 0 || applied < r.snapIndex+r.snapshotEvery {
		return
	}
	// Whatever happens next, wait another snapshotEvery entries before trying
	// again, so a persistent failure does not retry on every entry.
	r.snapIndex = applied

	data, err := r.sm.Snapshot()
	if err != nil {
		r.logger.Error("raft: the state machine failed to take a snapshot", "index", applied, "err", err)
		return
	}
	r.mu.Lock()
	err = r.node.Compact(applied, data)
	r.mu.Unlock()
	if err != nil {
		r.logger.Error("raft: cannot compact the log", "index", applied, "err", err)
		return
	}
	r.snapshotsTaken.Add(1)
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
			r.dropped.Add(1)
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
				r.sendFailures[to].Add(1)
				r.logger.Debug("raft send failed", "to", to, "type", m.Type.String(), "err", err)
			}
		}
	}
}
