# The kitchen service

`orderservice` takes commands from customers and staff over HTTP (place an order, pay it, start
and finish the work at a station, hand it over, cancel, receive goods), decides each one with
the rules of [`internal/kitchen`](README.md), and writes the resulting events to **one raftlog
topic**. It keeps nothing else: its orders and its stock are built by reading that topic, so a
service that is stopped and started again, or a new one on another machine, comes back with the
same state. The design and its costs are in [ADR 13](../adr/0013-order-service-on-one-log.md).

## Run it

You need a raftlog cluster. The quickest is the cluster console (it starts three nodes on
ports 19091 to 19093):

```bash
make build
./bin/raftlogconsole -spawn 3
```

Then the service, with the sample files of `examples/kitchen` (invented for trying it out):

```bash
./bin/orderservice -addr 127.0.0.1:19091,127.0.0.1:19092,127.0.0.1:19093 \
  -menu examples/kitchen/menu.json -promotions examples/kitchen/promotions.json \
  -users examples/kitchen/users.json -timezone Asia/Ho_Chi_Minh
```

It listens on `127.0.0.1:8100` and prints how many records it replayed. Run **one** service per
topic: it decides a command on the state it has built, so two writers could decide on state the
other has already changed.

| Flag | Meaning | Default |
|---|---|---|
| `-addr` | raftlog nodes, all of them (`RAFTLOG_ADDR`) | required |
| `-menu`, `-promotions`, `-users` | the files below (`ORDERSERVICE_MENU`, `_PROMOTIONS`, `_USERS`) | menu and users required |
| `-listen` | HTTP address (`ORDERSERVICE_LISTEN`) | `127.0.0.1:8100` |
| `-topic` | the topic of the log, created with one partition if missing (`ORDERSERVICE_TOPIC`) | `kitchen` |
| `-timezone` | the venue's zone; prices by time of day follow its clock (`ORDERSERVICE_TIMEZONE`) | `Local` |
| `-command-timeout` | time allowed to one command, including the wait for the log | `10s` |

## The files that describe the venue

All three are JSON, and a key the service does not know is an error, so a typo is reported at
start instead of silently doing nothing. None of it is stored in the log: to change it, edit the
file and restart. An order keeps the names and prices it was taken with.

- **Menu** (`examples/kitchen/menu.json`): items with a station, a price in whole đồng, a recipe
  in the ingredient's base unit (grams, millilitres, pieces), option groups (`min`/`max` choices,
  a price change and extra ingredients per option), price windows by time of day (`from`/`to` as
  `15:00`, optional `days`), combos made of one item per slot, and `low_at`, the quantity at or
  below which an ingredient is flagged as running low.
- **Promotions** (`examples/kitchen/promotions.json`): `percent_off` (in basis points),
  `fixed_off` and `buy_x_get_y`, with optional codes, a scope by category or item, a minimum, a
  cap, validity dates, `exclusive`, `priority` and `single_use`. How offers combine is in the
  [domain core](README.md).
- **Users** (`examples/kitchen/users.json`): `{id, token, roles, station}`. A token is at least
  16 characters, and an ID or a token cannot be used twice. A kitchen worker has a `station`.

This is the simplest authentication that makes the roles real, not a login system: there are no
passwords, sessions or expiry, and a token is as good as the file it is in. Never reuse the
sample tokens: they are in this repository.

## The API

JSON in and out. Everything except the menu and `/healthz` needs `Authorization: Bearer <token>`.
Money is a whole number of đồng. **Every command carries a `command_id`** (8 to 64 letters, digits,
dots, dashes or underscores) chosen by the caller: sending the same command again, after a lost
answer or a timeout, does not do it twice.

| Request | Who | Body (besides `command_id`) |
|---|---|---|
| `GET /api/menu` | anyone | the items for sale with today's price, and which are sold out |
| `GET /api/me` | anyone with a token | your ID, roles, station and permissions |
| `POST /api/orders` | customer (for themselves), cashier (at the counter) | `items`, `codes`, and for the cashier an optional `customer` |
| `GET /api/orders?status=paid,preparing&station=drinks&limit=50` | staff: all; customer: their own | |
| `GET /api/orders/{id}` | staff; the customer for their own | |
| `GET /api/orders/{id}/events` | manager, admin | the audit trail: who did what, when |
| `POST /api/orders/{id}/pay` | cashier | `method` (`cash`, `card`, `transfer`), `tendered` for cash, `reference` otherwise |
| `POST /api/orders/{id}/tickets/{station}/start`, `.../finish` | kitchen, at that station | |
| `POST /api/orders/{id}/handover` | cashier, runner | |
| `POST /api/orders/{id}/cancel` | customer, cashier, manager | `reason`; for an order that has not been paid |
| `POST /api/orders/{id}/cancel-request` | customer, cashier, manager | `reason`; for a paid order |
| `POST /api/orders/{id}/cancel-approve`, `cancel-reject` | manager (never for their own request) | `refund`; `reason` |
| `POST /api/orders/{id}/refund` | manager | `amount`, `reason`; for a completed order |
| `GET /api/stock` | kitchen, manager, storekeeper | |
| `POST /api/stock/receive`, `write-off`, `count` | storekeeper (`receive`); storekeeper, manager (the others) | `ingredient`, `qty`, `reason` (needed for `write-off` and `count`) |
| `GET /api/stream` | anyone with a token | server-sent events, see below |
| `GET /healthz` | anyone | `status`, number of orders, offset of the next record, skipped records |

An item in `items` is `{"item": "ca-phe-sua", "quantity": 2, "options": {"size": ["l"]}, "note": "..."}`,
or `{"combo": "combo-sang", "quantity": 1, "picks": [{"slot": 0, "item": "...", "options": {...}}, ...]}`.

**Errors** have a status and a stable `code` for screens: `400 invalid`, `401 unauthorized`,
`403 forbidden`, `404 not_found` (also for another customer's order, so that its existence is not
revealed), `409 conflict` (the order is not in a state that allows it, a cancellation waits, the
stock cannot cover a payment), `422 cart` (with `lines`: which line was refused and why),
`503 unavailable` (the log did not answer; `Retry-After` is set, and repeating the command is
safe). An out-of-stock payment lists the `shortages`.

### A session

```bash
B=http://127.0.0.1:8100; JSON='Content-Type: application/json'

# The storekeeper receives goods: stock starts empty and is part of the log like everything else.
curl -s -X POST $B/api/stock/receive -H "Authorization: Bearer demo-token-storekeeper-1" -H "$JSON" \
  -d '{"command_id":"recv-beans-0001","ingredient":"coffee-beans","qty":2000}'
# ... and likewise condensed-milk, ice, baguette, pate, cucumber.

# A customer orders two large coffees and a bánh mì, with a code.
curl -s -X POST $B/api/orders -H "Authorization: Bearer demo-token-customer-1" -H "$JSON" -d '{
  "command_id":"demo-order-0001", "codes":["HELLO10"],
  "items":[{"item":"ca-phe-sua","quantity":2,"options":{"size":["l"],"ice":["less-ice"]}},
           {"item":"banh-mi","quantity":1,"note":"no chili"}]}'
# -> 201 {"order":{"id":"ord-demo-order-0001","number":1,"status":"placed","subtotal":99000,
#         "discount":9900,"total":89100, ...,"tickets":[{"station":"drinks"...},{"station":"hot"...}]}}

# The cashier takes 100,000 in cash. The stock is taken now, not when the order was placed.
curl -s -X POST $B/api/orders/ord-demo-order-0001/pay -H "Authorization: Bearer demo-token-cashier-1" -H "$JSON" \
  -d '{"command_id":"demo-pay-0001","method":"cash","tendered":100000}'
# -> {"status":"paid","payment":{"change":10900,...}}

# Each station starts and finishes its part; the order is "ready" when both are done.
curl -s -X POST $B/api/orders/ord-demo-order-0001/tickets/drinks/start -H "Authorization: Bearer demo-token-kitchen-drinks" -H "$JSON" -d '{"command_id":"demo-tk-0001"}'

# A runner hands it over; a manager reads the audit trail.
curl -s -X POST $B/api/orders/ord-demo-order-0001/handover -H "Authorization: Bearer demo-token-runner-1" -H "$JSON" -d '{"command_id":"demo-ho-0001"}'
curl -s $B/api/orders/ord-demo-order-0001/events -H "Authorization: Bearer demo-token-manager-1"
```

A customer paying is refused with `403 {"code":"forbidden", "error":"order: this person may not do
that: needs order.record_payment"}`, and an item whose ingredients are not in stock is refused at
the cart with `422 {"code":"cart","lines":[{"line":1,"error":"menu: sold out: Trà đào"}]}`.

### The stream

`GET /api/stream` sends server-sent events for what the caller may see: a customer hears about
their own orders, staff about all of them, and whoever may read the stock hears that it changed.

```
retry: 2000

id: 13
event: order
data: {"number":2,"offset":13,"order":"ord-demo-order-0003","status":"placed","type":"order.placed"}
```

An event says that something changed, not what the screen should show: the screen reads the
state first (`GET /api/orders`), listens, and asks again for the order or the stock when told.
There are `stock` events too (`{"offset": n}`), keep-alive comments when idle, and an
`event: reset` before the stream is closed if the reader fell too far behind: read the state again
and reconnect. The token goes in the `Authorization` header, so a browser reads the stream with
`fetch`, not `EventSource` (which cannot set it).

## How it works, and what to know

- **The log.** One topic with one partition, so the log gives one order to everything. Each
  record is an event of an order (the same JSON as `order.Encode`) or a stock movement made by a
  person. The stock that an order takes (at payment) and gives back (on a cancellation before
  work began) is not written: it follows from the order events, so every reader derives it alike.
- **Deciding a command.** Under one lock the service reads the order from its state, asks the
  domain core whether the command is allowed, checks the stock for a payment or the movement for a
  stock command, appends the event, and applies it. Nothing is written for a command that is refused.
- **Order numbers** count from 1 in the order the log holds the orders. They are chosen when the
  order is placed, from the state, so unlike an offset they cannot be known before the write: this
  is why there must be one writer.
- **A write whose answer is lost.** If the log does not answer, the service says `503` and marks
  its state as possibly behind. It then writes a *barrier* (a record that changes nothing) and reads
  until it sees it, which puts everything written before it, including the command whose answer was
  lost, into the state: a plain read could be answered by a cluster node that is behind and would
  prove nothing. If the command is there it is not done twice, because its ID is the event's ID. A
  background loop does this without waiting for the next command, and the same barrier is written
  once when the service starts, so the log shows a barrier at every start.
- **A record that does not fit** (garbage, or an event written by something else that the order
  cannot take) is skipped by every reader in the same way and counted; `/healthz` shows the count
  and the service logs each one. A command of this service that turns out not to fit when it is
  applied in the order of the log is answered `409`, never as a success.
- **Starting** reads the whole log, so the start takes longer as the history grows. There are no
  snapshots of the service's state yet.

## What is not here

A login and accounts (the users file stands in), shifts and reports, editing the menu without a
restart, several writers or read-only replicas, snapshots of the state, and the screens. The
screens, and a test that kills the cluster's leader at the lunch rush, are the next branches.
