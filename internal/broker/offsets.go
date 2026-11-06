package broker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

const (
	offsetsDirName   = "offsets"
	maxGroupNameLen  = 249
	offsetReplaySize = 1024 // records read per call when replaying the offsets log
)

var groupNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var (
	ErrInvalidGroup     = errors.New("broker: invalid consumer group name")
	ErrOffsetBeyondEnd  = errors.New("broker: offset is beyond the end of the partition")
	errCorruptOffsetRec = errors.New("broker: corrupt record in the offsets log")
)

// OffsetInfo is the position of a consumer group in one partition.
type OffsetInfo struct {
	// Committed is false if the group has not committed an offset yet.
	Committed bool
	// Offset is the committed offset: the next offset the group will consume.
	// It is 0 if nothing was committed.
	Offset uint64
	// End is the offset the next produced message will get.
	End uint64
}

// Lag is the number of messages the group has not consumed yet.
func (i OffsetInfo) Lag() uint64 {
	if i.End < i.Offset {
		return 0
	}
	return i.End - i.Offset
}

type offsetKey struct {
	group     string
	topic     string
	partition int
}

// offsetStore keeps the committed offsets of consumer groups.
//
// Every commit is appended to a small storage.Log; the latest record for a key
// wins. The in-memory map is rebuilt by replaying that log when the broker
// opens. This is the same idea as Kafka's __consumer_offsets topic.
//
// Limitation: the log is never compacted, so it grows with every commit and
// opening time grows with it.
type offsetStore struct {
	mu      sync.Mutex
	log     *storage.Log
	offsets map[offsetKey]uint64
}

func openOffsetStore(dir string, opts storage.Options) (*offsetStore, error) {
	l, err := storage.Open(filepath.Join(dir, offsetsDirName), opts)
	if err != nil {
		return nil, fmt.Errorf("open offsets log: %w", err)
	}
	s := &offsetStore{log: l, offsets: make(map[offsetKey]uint64)}
	if err := s.replay(); err != nil {
		_ = l.Close()
		return nil, err
	}
	return s, nil
}

// replay rebuilds the in-memory map from the log.
func (s *offsetStore) replay() error {
	end := s.log.NextOffset()
	for next := uint64(0); next < end; {
		msgs, err := s.log.Read(next, offsetReplaySize)
		if err != nil {
			return fmt.Errorf("replay offsets log at %d: %w", next, err)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			k, off, err := decodeOffsetRecord(m)
			if err != nil {
				return fmt.Errorf("offsets log record %d: %w", m.Offset, err)
			}
			s.offsets[k] = off
		}
		next = msgs[len(msgs)-1].Offset + 1
	}
	return nil
}

// Record layout: key = "group\x00topic\x00partition", value = offset (u64, big-endian).
// Group and topic names cannot contain a NUL byte (see their name checks).
func encodeOffsetRecord(k offsetKey, offset uint64) storage.Message {
	key := k.group + "\x00" + k.topic + "\x00" + strconv.Itoa(k.partition)
	val := make([]byte, 8)
	binary.BigEndian.PutUint64(val, offset)
	return storage.Message{Key: []byte(key), Value: val}
}

func decodeOffsetRecord(m storage.Message) (offsetKey, uint64, error) {
	parts := strings.Split(string(m.Key), "\x00")
	if len(parts) != 3 || len(m.Value) != 8 {
		return offsetKey{}, 0, errCorruptOffsetRec
	}
	p, err := strconv.Atoi(parts[2])
	if err != nil {
		return offsetKey{}, 0, errCorruptOffsetRec
	}
	return offsetKey{group: parts[0], topic: parts[1], partition: p}, binary.BigEndian.Uint64(m.Value), nil
}

func (s *offsetStore) commit(k offsetKey, offset uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.log.Append(encodeOffsetRecord(k, offset)); err != nil {
		return fmt.Errorf("append to offsets log: %w", err)
	}
	s.offsets[k] = offset
	return nil
}

func (s *offsetStore) get(k offsetKey) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	off, ok := s.offsets[k]
	return off, ok
}

func (s *offsetStore) close() error { return s.log.Close() }

func validGroupName(name string) bool {
	return len(name) > 0 && len(name) <= maxGroupNameLen && groupNameRE.MatchString(name)
}

// CommitOffset records that the group has consumed everything before offset in
// the given partition, i.e. offset is the next offset it will read. Going back
// to an older offset is allowed (it replays messages). An offset beyond the
// end of the partition is rejected; the end itself is valid.
func (b *Broker) CommitOffset(group, name string, partition int, offset uint64) error {
	if !validGroupName(group) {
		return ErrInvalidGroup
	}
	t, err := b.lookup(name)
	if err != nil {
		return err
	}
	if partition < 0 || partition >= len(t.parts) {
		return ErrInvalidPartition
	}
	if end := t.parts[partition].log.NextOffset(); offset > end {
		return fmt.Errorf("%w: %d > %d", ErrOffsetBeyondEnd, offset, end)
	}
	return b.offsets.commit(offsetKey{group, name, partition}, offset)
}

// GroupOffset returns the position of a consumer group in one partition.
func (b *Broker) GroupOffset(group, name string, partition int) (OffsetInfo, error) {
	if !validGroupName(group) {
		return OffsetInfo{}, ErrInvalidGroup
	}
	t, err := b.lookup(name)
	if err != nil {
		return OffsetInfo{}, err
	}
	if partition < 0 || partition >= len(t.parts) {
		return OffsetInfo{}, ErrInvalidPartition
	}
	off, ok := b.offsets.get(offsetKey{group, name, partition})
	return OffsetInfo{
		Committed: ok,
		Offset:    off,
		End:       t.parts[partition].log.NextOffset(),
	}, nil
}
