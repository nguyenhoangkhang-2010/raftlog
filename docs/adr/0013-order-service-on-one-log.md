# 13. The kitchen service: orders and stock as events in one log

Date: 2026-11-13

## Status
Accepted

## Context
ADR 8 made the rules of the kitchen a pure, event-sourced core. A service now has to put them to
use on raftlog: take commands from people with roles, decide them, keep the events where they
survive the loss of a machine, and show screens a current view. Several things are not settled by
the core: where the stock lives, how orders are numbered, what happens when a write's answer is
lost, and what the service stores besides the log.

## Decision
- **One topic, one partition, and the service stores nothing else.** Every event of every order,
  and every stock movement a person makes, goes to the same partition, so the log gives one order
  to everything. The orders and the stock are state built by reading the log from the start; a
  service that is restarted, or started on another machine, rebuilds the same state. The topic
  must have exactly one partition, and the service refuses to start on one that has more.
- **The stock follows from the order events.** Payment takes the order's ingredients and a
  cancellation before the kitchen started gives them back, as ADR 8 says, and both are derived
  when the event is applied, not written as separate records. Only movements that a person makes
  (goods received, a write-off, a stocktake) are records of their own. Because the log has one
  order, "which of two orders gets the last coffee" is whichever payment comes first in the log,
  on every reader.
- **Decide, then append; one writer.** Under one lock the service asks the core (`Pay`,
  `StartTicket`, ...) whether the command is allowed on the order as it is, checks what the core
  cannot see (the stock for a payment, a copy of the stock for a movement), appends the event, and
  applies it to its state at the offset the log gave. Nothing is written for a refused command. This is correct
  with one writer. Raftlog has no conditional append, so two writers could each decide on state
  the other has just changed.
- **Idempotent by command ID.** Every command carries an ID chosen by the caller, which becomes the
  ID of the event. The first order is "ord-" plus the ID of the command that placed it. A repeat
  is recognised by the core (`Apply` ignores an event it has seen) and by the service (which
  returns the order that exists), so a client that times out can send the same command again.
- **A write with an unknown outcome marks the state stale.** If the log fails to answer, the
  command may or may not be stored. The service answers `503`, and before the next command it reads
  the log to the end. If a node that is itself behind answers that read, the command may be decided
  again and written a second time; the duplicate event has the same ID and is ignored when applied,
  and the stock is not taken twice.
- **Records that do not fit are skipped, counted and reported, never fatal.** A garbled record,
  an event for an order that does not exist, a payment that the order cannot take: every reader
  skips them in the same way, so readers stay in step and no order becomes impossible (the core
  checks every event again in `Apply`). The count is in `/healthz` and each skip is logged.
- **Order numbers are a counter in the state**, one more than the highest number seen. ADR 8
  expected numbers to come from the log offset, but an offset is known only after the write and the
  number is part of the event, so a counter is what a single writer can do. A number is unique and
  grows in log order; it does not restart each day.
- **What the venue is lives in files read at start**: the menu with recipes, the promotions, and the
  users. Nothing of it is in the log, and an order keeps the names and prices it was taken with,
  so editing the menu does not touch existing orders. The files refuse unknown keys.
- **Authentication is a list of people and secret tokens in a file**, compared in constant time.
  It makes the roles real, which is all the service needs now, and it is not a login system.
- **A stream of changes for screens**, as server-sent events masked to what the person may see; an
  event says that something changed, and the screen asks again for what it shows. A subscriber
  that falls behind is closed, not waited for.

## Consequences
- The service is thin and its state is disposable. The durability and the failover of the
  orders are the cluster's: a test starts real nodes, takes orders, restarts the service,
  kills the cluster's leader in the middle of taking orders and paying, and rebuilds the state in
  a third service.
- Starting reads the whole log, so the start takes longer as the history grows. Snapshots of the
  service's state would bound it and are not done.
- One writer is a single point of failure for commands (reads of the log still work). A standby
  that takes over would need a fence so that the old writer cannot write after it, and is not done.
- Throughput of the service is the throughput of one partition: every command is one write
  that waits for the cluster. Group commit (ADR 12) lets concurrent commands share the cost.
- Writing the service's tests found a rule of the domain core that was in its documentation and
  not in its code: a station could finish its work while a cancellation waited for the manager.
  `FinishTicket` and `Apply` now refuse it, and a second check, in `Apply`, that had no test, has one.
