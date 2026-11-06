package broker

import (
	"fmt"
	"os"
	"path/filepath"
)

// nextOffset returns the offset the next message of a partition will get.
func (b *Broker) nextOffset(name string, partition int) (uint64, error) {
	t, err := b.lookup(name)
	if err != nil {
		return 0, err
	}
	if partition < 0 || partition >= len(t.parts) {
		return 0, ErrInvalidPartition
	}
	return t.parts[partition].log.NextOffset(), nil
}

// reset throws away every topic, message and offset and leaves the broker empty
// and usable. Restoring a snapshot starts with it.
func (b *Broker) reset() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	for _, t := range b.topics {
		_ = t.close()
	}
	if b.offsets != nil {
		_ = b.offsets.close()
	}
	for _, d := range []string{filepath.Join(b.dir, topicsDirName), filepath.Join(b.dir, offsetsDirName)} {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("remove %s: %w", d, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(b.dir, topicsDirName), 0o755); err != nil {
		return err
	}
	offsets, err := openOffsetStore(b.dir, b.opts.Storage)
	if err != nil {
		return err
	}
	b.topics = make(map[string]*topic)
	b.offsets = offsets
	return nil
}

// all returns a copy of every committed offset.
func (s *offsetStore) all() map[offsetKey]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[offsetKey]uint64, len(s.offsets))
	for k, v := range s.offsets {
		out[k] = v
	}
	return out
}
