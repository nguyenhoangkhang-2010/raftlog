# Virtual waiting room for course registration

The first real application on top of raftlog. When registration opens, thousands of
students open the page in the same minute. Without a line, the registration system
falls over or serves whoever happens to get through. The waiting room puts everyone
in one fair line, lets them in a few at a time, and tells each person where they stand.

It is also a test of the broker: the line is only worth anything if it survives the
loss of a machine without anyone losing or swapping their place.

## How it works

```
 browser ──POST /api/join──►  waitingroom  ──Produce──►  topic "queue"  (one "joined" event per arrival)
    ▲                            │  ▲                          │
    │                            │  └─────── Fetch (long poll) ┤
    └──── server-sent events ────┘                             │
                                 ▲                       topic "gate"   ("admitted up to offset N")
        admission controller ────┘ Produce every interval
```

- **`queue`** holds one `joined` event per person. A person's place is the offset of
  their event: the order of one replicated log.
- **`gate`** holds `admitted` events. "Admitted up to N" lets in everyone whose join
  has an offset below N. The gate only moves forward.
- **The web process stores nothing.** It reads both topics and builds the line in
  memory; position = the number of people ahead who are not yet admitted, plus one.
  Restart it, or start a second one, and it rebuilds the same line.
- **Joining is idempotent.** The browser makes up a ticket ID and keeps it. Joining
  again with it, after a reload or a lost answer, returns the same place.
- **The admission rate and the wait estimate are measured** from the timestamps of
  recent admissions. Until two admissions have been seen, no estimate is shown.

The decisions and their costs are in [ADR 7](adr/0007-waiting-room-as-two-logs.md).

## Run it

Three raftlog nodes, then the waiting room (each in its own terminal):

```bash
raftlogd -node-id 1 -listen 127.0.0.1:9091 -data-dir data1 -peers 1=127.0.0.1:9091,2=127.0.0.1:9092,3=127.0.0.1:9093
raftlogd -node-id 2 -listen 127.0.0.1:9092 -data-dir data2 -peers 1=127.0.0.1:9091,2=127.0.0.1:9092,3=127.0.0.1:9093
raftlogd -node-id 3 -listen 127.0.0.1:9093 -data-dir data3 -peers 1=127.0.0.1:9091,2=127.0.0.1:9092,3=127.0.0.1:9093

waitingroom -addr 127.0.0.1:9091,127.0.0.1:9092,127.0.0.1:9093 -listen 127.0.0.1:8080 -admit-interval 5s -admit-batch 10
```

Open <http://127.0.0.1:8080>, enter a name and join. The page shows the whole line as a
grid of dots (let in, waiting, and you), a progress ring with your position, the measured
admission rate and wait estimate, and a clear "it is your turn" state. It follows the
system light or dark theme, works on a phone, and needs no JavaScript library.
To fill the line, simulate a crowd
in another terminal:

```bash
wrsim -url http://127.0.0.1:8080 -users 200 -arrival-rate 25 -seed 42
```

Now stop the node that is the leader (`raftlogctl -addr 127.0.0.1:9091 status` shows
who it is). The page keeps working and nobody loses their place.

Settings (flags, or the environment variables shown by `waitingroom -h`):

| Flag | Meaning | Default |
|---|---|---|
| `-addr` | raftlog node addresses (give them all) | `localhost:9092` |
| `-listen` | HTTP address | `:8080` |
| `-admit-interval`, `-admit-batch` | how fast people are let in | `5s`, `10` |
| `-controller` | run the admission controller (exactly one process per room) | `true` |
| `-queue-topic`, `-gate-topic` | topic names | `queue`, `gate` |

A second web-only process: `waitingroom -controller=false -listen :8081 ...`.

### HTTP API

| Request | Answer |
|---|---|
| `POST /api/join` `{"ticket":"…","user":"…"}` | the ticket's status; the same place if the ticket is already in the line |
| `GET /api/status?ticket=ID` | status, or 404 if the room does not know the ticket |
| `GET /api/watch?ticket=ID` | server-sent events: a `status` event now and after every change that affects the ticket |
| `GET /api/stats` | totals of the room |
| `GET /healthz` | 200 once the room has read its logs |

A ticket ID is 1–64 letters, digits, `-` or `_`. A status has `state` (`waiting`,
`admitted`, `unknown`), `position`, `ahead`, the totals and, when measured,
`admit_rate_per_s` and `eta_seconds`.

## What the tests show

| Test | Claim |
|---|---|
| `internal/waitingroom` state tests | places follow the queue order; the admitted are a prefix; a repeated join keeps the first place; the gate never moves back; the same logs give the same line in any reading order |
| `internal/waitingroom` room tests (real broker) | joins are idempotent under concurrency; a rebuilt room equals the original; a restarted controller reads the history first; the room rides through an outage of the log; an idle room waits instead of polling |
| `internal/waitingroom` HTTP tests | the API, error codes, and event streams that only speak when something changed |
| `cmd/wrsim` tests | a simulated crowd is admitted in order, and failures are reported |
| `test/chaos` `TestWaitingRoomKeepsItsOrderWhenTheLeaderDies` | on three real processes, with the leader killed while 120 people join: nobody is lost, nobody is invented, every place told to a person matches the log, the running room equals one rebuilt from the raw logs |

The code was also broken on purpose in 14 places (a repeated join taking a new place,
a gate that can move back, an off-by-one in the gate, a join that does not read its
own write, a controller that skips the history, a watcher that never rests, …), and
each was caught by at least one test.

## A measured run

One run on one Windows laptop, three `raftlogd` processes, `waitingroom -admit-interval
500ms -admit-batch 5`, `wrsim -users 100 -arrival-rate 25 -seed 42`, and the leader
killed three seconds after the crowd started to arrive (while people were still joining):

| | |
|---|---|
| people admitted | 100 of 100 |
| failed | 0 |
| join request, p50 / p95 / max | 15.3 ms / 28.6 ms / 34.7 ms |
| wait until admission, p50 / p95 / max | 2.6 s / 5.3 s / 5.4 s |
| positions that got worse | 0 |
| people admitted out of order (more than 1 s) | 0 |
| measured admission rate | 10.0 per second (5 every 500 ms, as configured) |

This is a single local run, not a benchmark: the waits are set mostly by the admission
interval and the batch, not by the system. Real numbers under load come in the
benchmark phase.

## Limits

- **One admission controller.** Nothing elects it; the operator runs one.
- **No identity.** A ticket is whatever the browser sends. One person can take many
  places, and a copied ticket is a copied place. A real deployment puts the
  university's sign-in in front and uses the student ID as the ticket.
- **No session after admission.** Being admitted is the end of this application:
  there is no expiry, no "give up my turn", no link to a registration system.
- **Memory and restart.** The line is held in memory and rebuilt by reading both
  topics from the start; fine for tens of thousands of people, not for millions.
- **Reads are not linearizable** (as everywhere in raftlog): a web process reading
  from a lagging node shows a slightly old line, never a wrong order.
