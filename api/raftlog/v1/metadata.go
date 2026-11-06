package raftlogv1

// Keys of the gRPC trailers that a node which is not the leader sends along with
// its FailedPrecondition error, so that a client can retry at the right place.
// They live here, next to the generated code, so that clients can use them
// without importing the server.
const (
	// LeaderIDKey carries the ID of the node the answering node believes is the
	// leader. It is absent if the node does not know.
	LeaderIDKey = "raftlog-leader-id"
	// LeaderAddrKey carries the address of that node, as the answering node
	// knows it. It is absent if the node cannot resolve one.
	LeaderAddrKey = "raftlog-leader-addr"
)
