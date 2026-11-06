// Package storage implements the append-only, segmented log with a sparse
// offset index and crash recovery (CRC32 checks).
//
// Planned API: Append(msg) (offset, error) and Read(offset, n) ([]Message, error).
package storage
