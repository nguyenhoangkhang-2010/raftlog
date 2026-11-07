# 6. Long-poll Fetch with a per-partition wake-up

Date: 2026-11-07

## Status
Accepted

## Context
A consumer that has read everything has two ways to learn about the next message:
ask again and again, or ask once and have the broker answer when there is
something. Asking again and again costs a request per consumer per interval and
still adds up to a whole interval of delay. An application that shows a live
queue position needs the message to arrive in milliseconds without hammering the
cluster.

## Decision
- **Long poll on the existing `Fetch`.** `FetchRequest.max_wait_ms` (default 0)
  says how long the server may hold the request when there is nothing to read at
  the offset. A request that finds data, or has no wait, behaves exactly as
  before, so old clients keep working. A wait that ends with no message is a
  normal empty response, not an error.
- **The server caps the wait** (`-max-fetch-wait`, default 30 s, 0 turns waiting
  off). A client cannot make the server hold a request for longer than the
  operator allows.
- **A wake-up channel per partition instead of polling inside the server.**
  `Watch(topic, partition)` returns a channel that is closed on the next append
  (and when the partition is closed or replaced). The handler subscribes *before*
  it reads, so a message that arrives between "found nothing" and "start waiting"
  closes a channel the handler already holds. Subscribing after the read would
  lose that message until the next one or until the timeout. A unit test places a
  message exactly in that gap.
- **The signal is sent where the data becomes readable.** For a standalone node
  that is the end of `Broker.Produce`; for a cluster node it is the end of the
  state machine's apply of a committed produce, so a reader on a follower wakes
  when the follower has applied the message, never earlier. Closing a broker, and a
  snapshot restore that replaces a partition, also wake the waiters, who look
  again and find the new state or an error.
- **Graceful shutdown releases waiters.** `Server.Shutdown` makes waiting fetches
  answer at once; otherwise `GracefulStop` would wait for them up to the cap.
- **Consumers still own their offsets.** Waiting changes how a consumer learns
  about data, not the delivery guarantee: it is still at-least-once.

## Consequences
- A reader sees a new message about as fast as the append is applied on the node it
  asks, with no polling interval. The numbers are measured in the benchmark phase,
  not claimed here.
- Every waiting request holds a goroutine and a gRPC stream slot on the node. There
  is no limit on the number of waiters besides the connection limits of gRPC, so a
  hostile client could hold many. The broker has no authentication or TLS (a listed
  limitation); a cap on waiters would be added together with them.
- A wake-up is per partition, so a busy partition wakes all its waiters on every
  append even if each wants a different offset; they check and go back to sleep.
  A per-offset index would avoid that and is not worth its cost at this scale.
- The caller's deadline must be longer than the wait, or the call fails with a
  deadline error instead of returning "nothing yet". The client library and
  `raftlogctl` say so.
