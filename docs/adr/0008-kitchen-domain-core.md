# 8. The kitchen domain as a pure, event-sourced core

Date: 2026-11-09

## Status
Accepted

## Context
The kitchen system (orders, kitchen stations, a till) has many roles and many rules:
who may pay, who may approve a cancellation, when stock goes back on the shelf, how
discounts combine. The rules will be used by a service on raftlog, by screens, and by
a recovery process that rebuilds orders from the log. If they live inside a handler or
a database transaction they cannot be tested as a whole, and they will be implemented
twice.

## Decision
- **A pure core with no I/O.** `internal/kitchen/...` imports nothing but the standard
  library and its own packages. Time comes in as an argument. Every rule can be
  tested without a database, a network or a clock.
- **An order is its events.** The state is the result of applying the events in order,
  so an order can be rebuilt from a log on any machine, and the log is the audit
  trail. There is no "update the status" anywhere.
- **Commands decide, `Apply` changes.** A command method checks who is asking and
  whether the order's state allows it, and returns the event; it changes nothing.
  `Apply` checks that an event fits the order and changes it. Live code does both;
  recovery does only `Apply`. `Apply` checks the structure again, so a corrupt log
  cannot produce an impossible order. A test requires that whatever a command
  accepts, `Apply` accepts.
- **Command IDs make retries safe.** The event takes the ID of its command. A repeat is
  recognised first and changes nothing, which absorbs the duplicates that
  at-least-once delivery will produce.
- **Money is a whole number of đồng**, with explicit bounds on price, quantity and
  lines so that no sum can overflow. Percentages are basis points. Splitting an
  amount uses the largest remainder method, so parts add up exactly.
- **Lines are snapshots.** Pricing copies names, prices, the pieces for each station and
  the ingredients into the order. The menu can change without touching orders.
- **Separation of duties is in the table.** The roles and permissions are one data
  structure, tested as a whole and printed into the documentation, where a test fails
  if the document and the code differ. The rule that nobody approves their own
  request is in the order, because only the order knows who asked.
- **Stock is a ledger of movements.** One method applies them, all-or-nothing, and a
  replay of the accepted movements gives the same stock. It is taken when an order is
  paid and given back only if the order is cancelled before work began.

## Consequences
- The service, the database and the screens in the next branches are thin: they call
  these functions and store the results. Their tests can be about plumbing.
- The rules are exercised by random walks as well as examples, which found the gaps
  in the first version of the test suite itself (a walk that never reached a refund
  was reported as proving too little, and was made smarter).
- Only what the code knows how to apply can be in the log. An event type that a newer
  version adds is refused by an older reader (unknown fields are an error), which is
  deliberate: half understanding an order is worse than stopping.
- Not decided here, and left to the service: how order numbers are assigned (from the
  log offset, so they are unique and in order), where the stock movements are written,
  and how the clock of the venue's time zone reaches the menu's time windows.
