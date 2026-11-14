package storage

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// Message is the unit of data stored in the log.
type Message struct {
	// Offset is assigned by the log on Append; any value set by the caller is ignored.
	Offset uint64
	// Timestamp is in milliseconds since the Unix epoch. If zero, Append fills it in.
	Timestamp int64
	Key       []byte
	Value     []byte
}

// On-disk record layout (all integers big-endian):
//
//	+-----------+-----------+-------------------------------------------+
//	| length u32| crc32 u32 | body (length bytes)                       |
//	+-----------+-----------+-------------------------------------------+
//
//	body = offset u64 | timestamp i64 | keyLen u32 | key | valLen u32 | value
//
// length is the size of body only. crc32 (Castagnoli) covers body only.
const (
	headerSize     = 8                // length + crc32
	bodyFixedSize  = 8 + 8 + 4 + 4    // offset + timestamp + keyLen + valLen
	maxRecordBytes = 64 * 1024 * 1024 // sanity limit on a single record body
	maxFieldBytes  = maxRecordBytes - bodyFixedSize
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	// ErrCorrupt means a record failed its checksum or has an impossible shape.
	ErrCorrupt = errors.New("storage: corrupt record")
	// errTornRecord means the data ends in the middle of a record
	// (typically a crash during a write).
	errTornRecord = errors.New("storage: torn record")
	// ErrRecordTooLarge is returned by Append when a message exceeds the size limit.
	ErrRecordTooLarge = errors.New("storage: record too large")
)

// recordSize returns the number of bytes the message occupies on disk.
func recordSize(m *Message) int64 {
	return int64(headerSize + bodyFixedSize + len(m.Key) + len(m.Value))
}

// encode serializes the message into its on-disk form.
func encode(m *Message) ([]byte, error) {
	if len(m.Key) > maxFieldBytes || len(m.Value) > maxFieldBytes ||
		bodyFixedSize+len(m.Key)+len(m.Value) > maxRecordBytes {
		return nil, ErrRecordTooLarge
	}
	bodyLen := bodyFixedSize + len(m.Key) + len(m.Value)
	buf := make([]byte, headerSize+bodyLen)

	body := buf[headerSize:]
	binary.BigEndian.PutUint64(body[0:8], m.Offset)
	binary.BigEndian.PutUint64(body[8:16], uint64(m.Timestamp))
	binary.BigEndian.PutUint32(body[16:20], uint32(len(m.Key)))
	n := 20 + copy(body[20:], m.Key)
	binary.BigEndian.PutUint32(body[n:n+4], uint32(len(m.Value)))
	copy(body[n+4:], m.Value)

	binary.BigEndian.PutUint32(buf[0:4], uint32(bodyLen))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(body, crcTable))
	return buf, nil
}

// readRecord reads one record from r. It returns the message and the number of
// bytes consumed.
//
// Errors:
//   - io.EOF: clean end of data, nothing was read.
//   - errTornRecord: data ended in the middle of a record.
//   - ErrCorrupt: checksum mismatch or invalid lengths.
func readRecord(r io.Reader) (Message, int64, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Message{}, 0, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Message{}, 0, errTornRecord
		}
		return Message{}, 0, err
	}

	bodyLen := binary.BigEndian.Uint32(hdr[0:4])
	wantCRC := binary.BigEndian.Uint32(hdr[4:8])
	if bodyLen < bodyFixedSize || bodyLen > maxRecordBytes {
		return Message{}, 0, ErrCorrupt
	}

	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Message{}, 0, errTornRecord
		}
		return Message{}, 0, err
	}
	if crc32.Checksum(body, crcTable) != wantCRC {
		return Message{}, 0, ErrCorrupt
	}

	m, err := decodeBody(body)
	if err != nil {
		return Message{}, 0, err
	}
	return m, int64(headerSize) + int64(bodyLen), nil
}

func decodeBody(body []byte) (Message, error) {
	var m Message
	m.Offset = binary.BigEndian.Uint64(body[0:8])
	m.Timestamp = int64(binary.BigEndian.Uint64(body[8:16]))

	keyLen := int(binary.BigEndian.Uint32(body[16:20]))
	pos := 20
	if keyLen < 0 || pos+keyLen+4 > len(body) {
		return Message{}, ErrCorrupt
	}
	m.Key = append([]byte(nil), body[pos:pos+keyLen]...)
	pos += keyLen

	valLen := int(binary.BigEndian.Uint32(body[pos : pos+4]))
	pos += 4
	if valLen < 0 || pos+valLen != len(body) {
		return Message{}, ErrCorrupt
	}
	m.Value = append([]byte(nil), body[pos:pos+valLen]...)
	return m, nil
}
