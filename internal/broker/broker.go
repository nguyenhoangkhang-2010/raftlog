package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// AnyPartition asks Produce to pick the partition itself.
const AnyPartition = -1

const (
	topicsDirName  = "topics"
	topicMetaName  = "topic.json"
	partitionFmt   = "partition-%04d"
	maxTopicNameLn = 249
)

var topicNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var (
	ErrTopicExists       = errors.New("broker: topic already exists")
	ErrTopicNotFound     = errors.New("broker: topic not found")
	ErrInvalidTopicName  = errors.New("broker: invalid topic name")
	ErrInvalidPartitions = errors.New("broker: invalid number of partitions")
	ErrInvalidPartition  = errors.New("broker: partition out of range")
	ErrEmptyBatch        = errors.New("broker: no messages to produce")
	ErrClosed            = errors.New("broker: closed")
)

// Options configures a Broker.
type Options struct {
	// Storage is passed to every partition log.
	Storage storage.Options
	// MaxPartitions limits the partitions of a single topic. It must be at least 1.
	MaxPartitions int
}

// TopicInfo describes a topic.
type TopicInfo struct {
	Name       string
	Partitions int
}

type topicMeta struct {
	Partitions int `json:"partitions"`
}

type partition struct {
	mu  sync.Mutex // keeps a produced batch contiguous in the log
	log *storage.Log

	// wake is closed, and replaced, whenever the partition may have changed:
	// after an append and when the partition is closed. See Watch.
	wakeMu sync.Mutex
	wake   chan struct{}
}

func newPartition(l *storage.Log) *partition {
	return &partition{log: l, wake: make(chan struct{})}
}

// signal wakes everyone who called Watch since the last signal.
func (p *partition) signal() {
	p.wakeMu.Lock()
	close(p.wake)
	p.wake = make(chan struct{})
	p.wakeMu.Unlock()
}

type topic struct {
	name  string
	parts []*partition
	rr    atomic.Uint64 // round-robin counter for messages without a key
}

// Broker manages topics, each split into partitions, each partition backed by
// one storage.Log. It is safe for concurrent use.
type Broker struct {
	mu     sync.RWMutex
	dir    string
	opts   Options
	topics map[string]*topic
	closed bool

	offsets *offsetStore
}

// Open opens the broker data under dir, loading every topic found there.
func Open(dir string, opts Options) (*Broker, error) {
	if opts.MaxPartitions < 1 {
		return nil, fmt.Errorf("broker: MaxPartitions must be at least 1, got %d", opts.MaxPartitions)
	}
	root := filepath.Join(dir, topicsDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create topics dir: %w", err)
	}

	b := &Broker{dir: dir, opts: opts, topics: make(map[string]*topic)}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read topics dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := b.loadTopic(e.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue // directory without topic.json: an unfinished or foreign directory
		}
		if err != nil {
			_ = b.Close()
			return nil, fmt.Errorf("load topic %q: %w", e.Name(), err)
		}
		b.topics[t.name] = t
	}

	offsets, err := openOffsetStore(dir, opts.Storage)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	b.offsets = offsets
	return b, nil
}

func (b *Broker) topicDir(name string) string {
	return filepath.Join(b.dir, topicsDirName, name)
}

// loadTopic opens a topic from disk. Missing partition directories are
// created, which also completes a CreateTopic that crashed halfway.
func (b *Broker) loadTopic(name string) (*topic, error) {
	data, err := os.ReadFile(filepath.Join(b.topicDir(name), topicMetaName))
	if err != nil {
		return nil, err
	}
	var meta topicMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse %s: %w", topicMetaName, err)
	}
	if meta.Partitions < 1 {
		return nil, fmt.Errorf("%s: invalid partition count %d", topicMetaName, meta.Partitions)
	}
	return b.openPartitions(name, meta.Partitions)
}

func (b *Broker) openPartitions(name string, n int) (*topic, error) {
	t := &topic{name: name}
	for i := 0; i < n; i++ {
		dir := filepath.Join(b.topicDir(name), fmt.Sprintf(partitionFmt, i))
		l, err := storage.Open(dir, b.opts.Storage)
		if err != nil {
			t.close()
			return nil, fmt.Errorf("open partition %d: %w", i, err)
		}
		t.parts = append(t.parts, newPartition(l))
	}
	return t, nil
}

func (t *topic) close() error {
	var errs []error
	for _, p := range t.parts {
		errs = append(errs, p.log.Close())
		p.signal() // let a waiting Fetch see that the partition is gone
	}
	return errors.Join(errs...)
}

func validTopicName(name string) bool {
	return len(name) > 0 && len(name) <= maxTopicNameLn &&
		name != "." && name != ".." && topicNameRE.MatchString(name)
}

// CreateTopic creates a topic with the given number of partitions.
func (b *Broker) CreateTopic(name string, partitions int) error {
	if !validTopicName(name) {
		return ErrInvalidTopicName
	}
	if partitions < 1 || partitions > b.opts.MaxPartitions {
		return fmt.Errorf("%w: %d (allowed 1..%d)", ErrInvalidPartitions, partitions, b.opts.MaxPartitions)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if _, ok := b.topics[name]; ok {
		return ErrTopicExists
	}

	dir := b.topicDir(name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrTopicExists
		}
		return fmt.Errorf("create topic dir: %w", err)
	}
	t, err := b.writeMetaAndOpen(name, dir, partitions)
	if err != nil {
		_ = os.RemoveAll(dir) // we created it a moment ago
		return err
	}
	b.topics[name] = t
	return nil
}

func (b *Broker) writeMetaAndOpen(name, dir string, partitions int) (*topic, error) {
	data, err := json.Marshal(topicMeta{Partitions: partitions})
	if err != nil {
		return nil, err
	}
	// Write to a temp file and rename so topic.json is either absent or complete.
	tmp := filepath.Join(dir, topicMetaName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("write topic meta: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, topicMetaName)); err != nil {
		return nil, fmt.Errorf("commit topic meta: %w", err)
	}
	return b.openPartitions(name, partitions)
}

func (b *Broker) lookup(name string) (*topic, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, ErrClosed
	}
	t, ok := b.topics[name]
	if !ok {
		return nil, ErrTopicNotFound
	}
	return t, nil
}

// choose picks a partition: by key hash when the key is set, else round-robin.
func (t *topic) choose(key []byte) int {
	n := uint64(len(t.parts))
	if len(key) == 0 {
		return int((t.rr.Add(1) - 1) % n)
	}
	h := fnv.New32a()
	_, _ = h.Write(key)
	return int(uint64(h.Sum32()) % n)
}

// Produce appends msgs to one partition of the topic and returns the partition
// used and the offset of the first message; the others follow contiguously.
//
// If partition is AnyPartition, the partition is chosen from the key of the
// first message. All messages in one call go to the same partition.
//
// A batch is not atomic: if an append fails midway, earlier messages stay in
// the log and the error says how many were written.
func (b *Broker) Produce(name string, partition int, msgs []storage.Message) (int, uint64, error) {
	if len(msgs) == 0 {
		return 0, 0, ErrEmptyBatch
	}
	t, err := b.lookup(name)
	if err != nil {
		return 0, 0, err
	}

	idx := partition
	if idx == AnyPartition {
		idx = t.choose(msgs[0].Key)
	} else if idx < 0 || idx >= len(t.parts) {
		return 0, 0, ErrInvalidPartition
	}

	p := t.parts[idx]
	p.mu.Lock()
	defer p.mu.Unlock()
	// Wake waiting readers even if the batch failed midway: the messages that
	// were written are readable. The signal goes out before p.mu is released,
	// so a reader that was woken and reads at once sees the whole batch.
	defer p.signal()
	var base uint64
	for i, m := range msgs {
		off, err := p.log.Append(m)
		if err != nil {
			return idx, 0, fmt.Errorf("append message %d of %d (%d written): %w", i+1, len(msgs), i, err)
		}
		if i == 0 {
			base = off
		}
	}
	return idx, base, nil
}

// Fetch reads up to limit messages from a partition starting at offset.
func (b *Broker) Fetch(name string, partition int, offset uint64, limit int) ([]storage.Message, error) {
	t, err := b.lookup(name)
	if err != nil {
		return nil, err
	}
	if partition < 0 || partition >= len(t.parts) {
		return nil, ErrInvalidPartition
	}
	return t.parts[partition].log.Read(offset, limit)
}

// Watch returns a channel that is closed the next time the partition may have
// new data, or when it is closed or replaced. It is how a reader waits for
// messages without polling.
//
// To avoid missing a message that arrives in between, call Watch first, then
// Fetch; if the Fetch finds nothing, wait on the channel and start over. A woken
// reader may find nothing new, so it must check again.
func (b *Broker) Watch(name string, partition int) (<-chan struct{}, error) {
	t, err := b.lookup(name)
	if err != nil {
		return nil, err
	}
	if partition < 0 || partition >= len(t.parts) {
		return nil, ErrInvalidPartition
	}
	p := t.parts[partition]
	p.wakeMu.Lock()
	defer p.wakeMu.Unlock()
	return p.wake, nil
}

// Topics lists all topics, sorted by name.
func (b *Broker) Topics() []TopicInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]TopicInfo, 0, len(b.topics))
	for _, t := range b.topics {
		out = append(out, TopicInfo{Name: t.name, Partitions: len(t.parts)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close closes every partition log.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var errs []error
	for _, t := range b.topics {
		errs = append(errs, t.close())
	}
	if b.offsets != nil {
		errs = append(errs, b.offsets.close())
	}
	return errors.Join(errs...)
}
