package storage

// ValidateMessage reports whether m is small enough to be stored (it returns
// ErrRecordTooLarge if not). Callers that must not half-apply a batch use it to
// refuse a bad message before writing anything.
func ValidateMessage(m Message) error {
	if len(m.Key) > maxFieldBytes || len(m.Value) > maxFieldBytes ||
		bodyFixedSize+len(m.Key)+len(m.Value) > maxRecordBytes {
		return ErrRecordTooLarge
	}
	return nil
}
