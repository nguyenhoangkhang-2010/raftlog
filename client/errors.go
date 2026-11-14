package client

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Errors a Client returns. Test for them with errors.Is; the message after the
// colon is the cluster's own explanation.
var (
	// ErrNotFound: the topic, or the partition of a topic, does not exist.
	ErrNotFound = errors.New("raftlog: not found")
	// ErrAlreadyExists: for example, creating a topic that exists.
	ErrAlreadyExists = errors.New("raftlog: already exists")
	// ErrInvalidArgument: the request was refused as malformed, for example a
	// topic name with a slash or an empty batch.
	ErrInvalidArgument = errors.New("raftlog: invalid argument")
	// ErrOffsetOutOfRange: an offset beyond the end of a partition.
	ErrOffsetOutOfRange = errors.New("raftlog: offset out of range")
	// ErrUnavailable: no node could serve the request. The cluster may be down,
	// or without a leader for longer than the request was allowed to wait.
	ErrUnavailable = errors.New("raftlog: cluster unavailable")
)

// translate turns a gRPC error into one of the errors above, or into the
// context's own error. Anything else is returned unchanged.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var gs interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &gs) {
		return err
	}
	st := gs.GRPCStatus()
	// A bare status error says everything in its message. One that was wrapped
	// on the way, for example "gave up after 4 attempts", has more to say, so
	// keep the whole text.
	msg := st.Message()
	if _, bare := err.(interface{ GRPCStatus() *status.Status }); !bare {
		msg = err.Error()
	}
	wrap := func(sentinel error) error { return fmt.Errorf("%w: %s", sentinel, msg) }
	switch st.Code() {
	case codes.NotFound:
		return wrap(ErrNotFound)
	case codes.AlreadyExists:
		return wrap(ErrAlreadyExists)
	case codes.InvalidArgument:
		return wrap(ErrInvalidArgument)
	case codes.OutOfRange:
		return wrap(ErrOffsetOutOfRange)
	case codes.Unavailable, codes.FailedPrecondition:
		return wrap(ErrUnavailable)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
	case codes.Canceled:
		return fmt.Errorf("%w: %s", context.Canceled, msg)
	default:
		return err
	}
}
