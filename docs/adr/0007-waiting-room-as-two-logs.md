# 7. The waiting room as two logs and a projection

Date: 2026-11-08

## Status
Accepted, and later **retired**: the waiting room was removed from the repository (its code, its
commands and its document) once the kitchen system had become the application that depends on the
broker, so that the project tells one story instead of two. The decision below is kept as it was
made; the code is in the git history before that change.

## Context
The project needs an application that someone could use, and that needs what a
replicated log is good at. A virtual waiting room for course registration does:
when registration opens, thousands of students arrive in the same minute. The
line must be fair (first come, first served), survive the loss of a machine
without anyone losing their place, and show each person where they stand without
every browser hammering the servers.

## Decision
- **The whole state is two topics with one partition each.** `queue` gets one
  `joined` event per arrival; `gate` gets `admitted` events. The place in line is
  the offset of the `joined` event, so the order is the order of one replicated
  log, which the cluster already makes durable and agreed. One partition is
  deliberate: several partitions would have no common order.
- **The web process keeps nothing of its own.** It reads both topics from the start
  and builds the line in memory (`State`). A restart, or a second web process,
  rebuilds the same line. A test applies the same two logs in 50 random
  interleavings and requires the same line every time.
- **The gate is a pointer that only moves forward.** An `admitted` event says "every
  ticket that joined below queue offset N is admitted". Applying it is `max`, so a
  repeated, late or stale event cannot let anyone in twice or take anyone back, and
  the admitted tickets are always a prefix of the line.
- **Ticket IDs come from the caller and joining is idempotent.** The first `joined`
  event of an ID counts; later ones change nothing. A browser that retries after a
  lost answer, a double click, and the at-least-once duplicates of the client
  library all end up with one place. The browser keeps its ticket in local storage,
  so a reload keeps the place.
- **A join returns after the process has read its own write back.** The answer
  includes the position, and the position is derived from the log, so `Join` waits
  until the `joined` event has been applied locally.
- **One admission controller.** It reads the whole history first, then every
  interval writes one `admitted` event for the next batch. Two controllers would
  admit twice as fast but never out of order or twice. Choosing one automatically
  would need leader election in the application, which is out of scope.
- **Updates are pushed with server-sent events** over plain HTTP, from the long-poll
  `Fetch` (ADR 6): a change in the logs reaches the browser without polling. A
  stream sends a message only when the ticket's own status differs from the last
  one. Server-sent events reconnect by themselves and need no library.
- **Numbers shown are measured.** The admission rate comes from the timestamps of
  the last few `admitted` events; the wait estimate is the position divided by that
  rate, and absent until a rate exists.

## Consequences
- Surviving a node loss costs the application nothing: it uses the client library,
  which follows the leader. A test kills the leader of a real three-process cluster
  while 120 people join and checks the line against the raw logs.
- Memory grows with the number of tickets (a few hundred bytes each), and a restart
  replays both logs. Both are fine for tens of thousands of people and would need a
  snapshot of the projection beyond that.
- Nothing stops one person from taking many places: there is no login, rate limit or
  bot protection, and the ticket in the browser is a bearer token. A real deployment
  would sit behind the university's sign-in and use the student ID as the ticket.
- Admitted people are not tracked after admission: there is no session, no expiry and
  no "give up your turn". The admission is the end of this application's job.
