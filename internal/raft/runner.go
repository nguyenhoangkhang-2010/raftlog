package raft

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Transport sends one message to the node named in m.To. Delivery is best
// effort: Raft copes with lost, delayed and duplicated messages, so Send may
// fail without anyone retrying.
type Transport interface {
	Send(ctx context.Context, m Message) error
}

// peerQueueSize bounds the messages waiting for one slow or dead peer. When it
// is full the oldest message is dropped: a newer heartbeat is worth more than
// an old one.
const peerQueueSize = 64

// Runner drives a Node with a real clock and a real Transport.
//
// A goroutine calls Tick every tick interval, incoming messages arrive through
// Receive, and one goroutine per peer sends the outgoing messages so that a
// slow or dead peer never delays the others or the protocol itself.
type Runner struct {
	mu   sync.Mutex // guards node
	node *Node

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
func NewRunner(cfg Config, tr Transport, tickInterval time.Duration) (*Runner, error) {
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
		tr:          tr,
		tick:        tickInterval,
		sendTimeout: tickInterval * time.Duration(cfg.ElectionTicks), // never outlive an election timeout
		logger:      cfg.Logger,
		queues:      make(map[NodeID]chan Message),
		stop:        make(chan struct{}),
	}
	if r.logger == nil {
		r.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, p := range node.peers {
		r.queues[p] = make(chan Message, peerQueueSize)
	}
	return r, nil
}

// Start begins ticking and sending. It does nothing if already started.
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
	r.wg.Add(1)
	go r.loop()
}

// Stop stops the goroutines and waits for them. It is safe to call twice.
func (r *Runner) Stop() {
	r.stopped.Do(func() { close(r.stop) })
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
	msgs := r.node.Messages()
	r.mu.Unlock()

	r.dispatch(msgs)
	return err
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
			msgs := r.node.Messages()
			r.mu.Unlock()

			if err != nil {
				r.logger.Error("raft tick failed", "err", err)
			}
			r.dispatch(msgs)
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
