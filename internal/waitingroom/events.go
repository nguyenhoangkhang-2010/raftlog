package waitingroom

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Event types. The room is nothing but two logs of these events:
//
//	queue: one "joined" event per arrival. Its offset in the log is the place in line.
//	gate:  "admitted" events that move the gate forward. Everyone whose "joined"
//	       event has a smaller offset than the gate may go in.
//
// Every number the room shows is derived from these two logs, so it can be
// rebuilt on any machine by reading them from the start.
const (
	typeJoined   = "joined"
	typeAdmitted = "admitted"
)

// Limits on what a client may send.
const (
	maxUserRunes = 100
)

var (
	// ErrInvalidTicket: the ticket ID is empty, too long or has other characters
	// than letters, digits, '-' and '_'.
	ErrInvalidTicket = errors.New("waitingroom: invalid ticket ID")
	// ErrInvalidUser: the user label is too long or not valid UTF-8 text.
	ErrInvalidUser = errors.New("waitingroom: invalid user label")
)

var ticketRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// joinedEvent is stored in the queue log.
type joinedEvent struct {
	Type   string `json:"type"`
	Ticket string `json:"ticket"`
	User   string `json:"user,omitempty"`
}

// admittedEvent is stored in the gate log: every ticket that joined at a queue
// offset below UpTo is admitted. Count is how many tickets this event let in, as
// the admission controller saw it; it only feeds the measured admission rate.
type admittedEvent struct {
	Type  string `json:"type"`
	UpTo  uint64 `json:"upto"`
	Count int    `json:"count"`
}

func validTicket(id string) bool { return ticketRE.MatchString(id) }

// cleanUser trims a user label and checks it.
func cleanUser(user string) (string, error) {
	user = strings.TrimSpace(user)
	if !utf8.ValidString(user) || utf8.RuneCountInString(user) > maxUserRunes || strings.ContainsFunc(user, isControl) {
		return "", fmt.Errorf("%w: at most %d characters of plain text", ErrInvalidUser, maxUserRunes)
	}
	return user, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func encodeJoined(ticket, user string) ([]byte, error) {
	return json.Marshal(joinedEvent{Type: typeJoined, Ticket: ticket, User: user})
}

func encodeAdmitted(upTo uint64, count int) ([]byte, error) {
	return json.Marshal(admittedEvent{Type: typeAdmitted, UpTo: upTo, Count: count})
}

// decodeJoined parses a queue record. It reports false for anything that is not
// a well-formed "joined" event; such records are skipped, not fatal, so that one
// bad message cannot stop the room.
func decodeJoined(value []byte) (joinedEvent, bool) {
	var e joinedEvent
	if err := json.Unmarshal(value, &e); err != nil || e.Type != typeJoined || !validTicket(e.Ticket) {
		return joinedEvent{}, false
	}
	return e, true
}

func decodeAdmitted(value []byte) (admittedEvent, bool) {
	var e admittedEvent
	if err := json.Unmarshal(value, &e); err != nil || e.Type != typeAdmitted {
		return admittedEvent{}, false
	}
	return e, true
}
