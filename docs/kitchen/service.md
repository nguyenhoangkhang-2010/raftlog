# The kitchen service

`orderservice` takes commands from customers and staff over HTTP (place an order, pay it, start
and finish the work at a station, hand it over, cancel, receive goods), decides each one with
the rules of [`internal/kitchen`](README.md), and writes the resulting events to **one raftlog
topic**. Its orders and its stock are built by reading that topic, so a service that is stopped
and started again, or a new one on another machine, comes back with the same state. The people
who may use it are accounts in a PostgreSQL database (or, to try it out, a file of tokens). The
design and its costs are in [ADR 13](../adr/0013-order-service-on-one-log.md) for the orders and
[ADR 14](../adr/0014-accounts-in-postgresql.md) for the accounts.

## Run it

You need a raftlog cluster. The quickest is the cluster console (it starts three nodes on
ports 19091 to 19093):

```bash
make build
./bin/raftlogconsole -spawn 3
```

There are two ways for people to sign in, and the service takes one of them:

**With accounts** (a PostgreSQL database; people sign in with a login name and a password). This is
the way to run it for real. The address comes from `ORDERSERVICE_DATABASE_URL` (prefer it to the
flag: a flag shows in the process list). The tables are created at start.

```bash
export ORDERSERVICE_DATABASE_URL='postgres://kitchen_app:PASSWORD@127.0.0.1:5432/raftlog_kitchen'

# the first administrator, once: the password is asked for twice, without echo
./bin/orderservice user create -id admin -name "Administrator" -roles admin

./bin/orderservice -addr 127.0.0.1:19091,127.0.0.1:19092,127.0.0.1:19093 \
  -menu examples/kitchen/menu.json -promotions examples/kitchen/promotions.json \
  -timezone Asia/Ho_Chi_Minh
```

Open `http://127.0.0.1:8100/`, sign in as the administrator and make the other accounts on the
**Users** screen. [ADR 14](../adr/0014-accounts-in-postgresql.md) says what it does and what it
costs.

**With a file of tokens**, for trying the service without a database (and what the tests of the
service and the chaos test use). The sample files of `examples/kitchen` are invented for this; their
tokens never expire and are in this repository:

```bash
./bin/orderservice -addr 127.0.0.1:19091,127.0.0.1:19092,127.0.0.1:19093 \
  -menu examples/kitchen/menu.json -promotions examples/kitchen/promotions.json \
  -users examples/kitchen/users.json -timezone Asia/Ho_Chi_Minh
```

It listens on `127.0.0.1:8100` and prints how many records it replayed. Run **one** service per
topic: it decides a command on the state it has built, so two writers could decide on state the
other has already changed.

### Accounts from the command line

`orderservice user create`, `set-password` and `list` work on the database directly. They are how
the first administrator is made, and how an account is recovered when every administrator is locked
out or has forgotten the password (there is no email, so no "forgot password" link: an administrator
resets it on the Users screen, or this does).

```bash
./bin/orderservice user create -id cash-1 -name "Thu Ngan" -roles cashier,runner
./bin/orderservice user create -id cook-1 -name "Bep do uong" -roles kitchen -station drinks
./bin/orderservice user set-password -id admin          # clears a lock and ends the sessions
./bin/orderservice user list
```

| Flag | Meaning | Default |
|---|---|---|
| `-addr` | raftlog nodes, all of them (`RAFTLOG_ADDR`) | required |
| `-menu`, `-promotions` | the files below (`ORDERSERVICE_MENU`, `_PROMOTIONS`) | menu required |
| `-database-url` | PostgreSQL that holds the accounts (`ORDERSERVICE_DATABASE_URL`) | one of this and `-users` |
| `-users` | the file of tokens, for trying the service (`ORDERSERVICE_USERS`) | one of this and `-database-url` |
| `-session-idle`, `-session-max` | a session ends after this long without use, and this long after sign-in at the latest (`ORDERSERVICE_SESSION_IDLE`, `_MAX`) | `12h`, `168h` |
| `-secure-cookies` | mark the session cookie `Secure` although the service listens on plain HTTP, behind a proxy that does TLS (`ORDERSERVICE_SECURE_COOKIES`) | off |
| `-no-migrate` | do not bring the database schema up to date at start | off |
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
- **Users** of the token mode (`examples/kitchen/users.json`): `{id, token, roles, station}`. A
  token is at least 16 characters, and an ID or a token cannot be used twice. A kitchen worker has a
  `station`. With accounts there is no such file: the people are in the database.

The file of tokens is the simplest authentication that makes the roles real, and it is not a login
system: no passwords, sessions or expiry, and a token is as good as the file it is in. Never reuse
the sample tokens: they are in this repository.

## Accounts

With `-database-url`, people have a login name (3 to 32 characters, lower case, fixed for life; it
is also the name that orders and the audit trail carry), a password, one or more roles and, for a
kitchen worker, a station. What the service guarantees, and the limits of each:

- **Passwords** are argon2id hashes, at least ten characters, not containing the login name, not one of
  the most used. A temporary password (a new account, a reset) is random, shown once to the
  administrator, and must be changed at the first sign-in: until then the session can do nothing else.
- **A session** is a cookie that the page's script cannot read (`HttpOnly`, `SameSite=Strict`). It ends
  after 12 hours without use and 7 days after sign-in at the latest, when the person signs out, when
  the account is disabled or its password reset or changed (the others end; the one that changed it
  stays), or when an administrator ends it. People see and end their own sessions on **Account**.
- **Roles are read at every request**, so taking a role away or disabling an account counts at once. A
  screen that is open is told within 15 seconds.
- **Five wrong passwords in a row lock the account for 15 minutes**; an administrator can unlock it.
  An address may try 30 times a minute. A wrong password, an unknown name and a disabled account get
  the same answer.
- **The last administrator cannot be disabled or demoted**, and nobody changes their own roles or
  disables or resets their own account.
- **The trail** (`account_events`) records sign-ins, locks, password changes and what was done to whom,
  and cannot be updated or deleted from.

What is not there: email (an administrator resets a password), TLS (use a proxy and `-secure-cookies`),
a second factor, a check against known breaches, and limits shared between instances. Behind a proxy
all people share its address for the limit on sign-ins. [ADR 14](../adr/0014-accounts-in-postgresql.md)
has the reasons.

## The API

JSON in and out. Everything except the menu, `/healthz` and `/api/auth` needs a person who is signed in:
with accounts, the session cookie that `POST /api/login` sets; in token mode, `Authorization: Bearer
<token>`. Money is a whole number of đồng. **Every command carries a `command_id`** (8 to 64 letters, digits,
dots, dashes or underscores) chosen by the caller: sending the same command again, after a lost
answer or a timeout, does not do it twice.

| Request | Who | Body (besides `command_id`) |
|---|---|---|
| `GET /api/menu` | anyone | the items for sale with today's price, and which are sold out |
| `GET /api/auth` | anyone | `{"mode": "password"}` with accounts, `"token"` otherwise |
| `GET /api/me` | anyone signed in | your ID, name, roles, station, permissions, whether you must change your password, when the session ends |
| `POST /api/login` | anyone | `username`, `password`; sets the cookie. `401 bad_credentials`, `429 locked` or `rate_limited` |
| `POST /api/logout` | anyone signed in | ends the session and clears the cookie |
| `POST /api/me/password` | anyone signed in | `current`, `new`; ends the person's other sessions |
| `GET /api/me/sessions`, `POST /api/me/sessions/revoke-others`, `POST /api/me/sessions/{id}/revoke` | anyone signed in | their own sessions |
| `GET /api/admin/users`, `GET /api/admin/roles`, `GET /api/admin/events?subject=&limit=` | `user.manage` (administrator) | the accounts, the roles with their permissions, the trail |
| `POST /api/admin/users` | administrator | `id`, `display_name`, `roles`, `station`; the answer has the temporary password, once |
| `PUT /api/admin/users/{id}` | administrator | `display_name`, `roles`, `station` |
| `POST /api/admin/users/{id}/disable`, `enable`, `unlock`, `reset-password`, `revoke-sessions`; `GET .../sessions` | administrator | |
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
| `GET /api/stream` | anyone signed in | server-sent events, see below |
| `GET /healthz` | anyone | `status`, number of orders, offset of the next record, skipped records, and with accounts whether the database answers |

An item in `items` is `{"item": "ca-phe-sua", "quantity": 2, "options": {"size": ["l"]}, "note": "..."}`,
or `{"combo": "combo-sang", "quantity": 1, "picks": [{"slot": 0, "item": "...", "options": {...}}, ...]}`.

**Errors** have a status and a stable `code` for screens: `400 invalid`, `401 unauthorized`,
`403 forbidden`, `404 not_found` (also for another customer's order, so that its existence is not
revealed), `409 conflict` (the order is not in a state that allows it, a cancellation waits, the
stock cannot cover a payment), `422 cart` (with `lines`: which line was refused and why),
`503 unavailable` (the log did not answer, or with accounts the database did not; `Retry-After`
is set, and repeating the command is safe). An out-of-stock payment lists the `shortages`. With
accounts there are also `403 password_change_required` (a temporary password must be changed
first), `403 cross_site` (a request made by another site), `401 bad_credentials`, `429 locked` and
`429 rate_limited` (with `Retry-After`).

### A session

In token mode, with the sample tokens (with accounts, sign in with `curl -c jar -d '{"username":...,
"password":...}' .../api/login` and send `-b jar`, and the commands are the same):

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

## The screens

Open `http://127.0.0.1:8100/` in a browser. The service itself serves the screens, so there is
nothing else to run or install. With accounts the page asks for a login name and a password and
shows the screens that the person's roles allow; the session is a cookie, so the page keeps
nothing readable by script, and it survives a restart of the service. In token mode it asks for a
token (the ones in `users.json`) and keeps it in `sessionStorage` of that tab, so closing the tab
signs out. A browser holds one session per site: two people on one browser profile take turns.

| Screen | Who | What it does |
|---|---|---|
| **Order** | customer; cashier (at the counter, with an optional customer ID) | the menu with today's prices, options and combos, a cart, a promotion code, then "Place order". The price shown while choosing is an estimate: the total and any offer come from the service when the order is placed |
| **My orders** | customer | their orders with a live status, a note when the order is ready, "Cancel" before payment and "Ask to cancel" after |
| **Till** | cashier, runner | three columns: to pay, in the kitchen, ready to hand over. Payment by cash (with the change, and quick amounts), card or transfer (with a reference); "Hand over" |
| **Kitchen** | kitchen, for their own station | what is waiting and what is being made, with customer notes. When a cancellation is waiting for the manager, "Done" is held back and the card says so |
| **Manager** | manager | the day's counts and money (taken, refunded, net), cancellation requests with the refund that the rules allow (from 0 to the total once work began, the whole total before), refunds for completed orders, and a lookup of one order's audit trail |
| **Stock** | storekeeper (receive, write off, count); kitchen and manager (look) | the quantity of each ingredient, flagged when low or out |
| **Users** | administrator | the accounts with their state (active, must change password, locked, disabled), a form to create and edit them (roles; a station for kitchen workers), and per account: unlock, disable or enable, reset password, end sessions, history. A temporary password is shown once, with a copy button. Below, the trail of sign-ins and changes. On a phone the table becomes a stack of cards |
| **Account** | everyone, with accounts | who you are, a form to change your password, and where you are signed in, with a way to end the other sessions |

How it is built, and why it can be trusted with what customers type:

- **Four static files, an icon and two fonts, embedded in the program** (`internal/orderservice/web`):
  no framework, no build step, no network request to anywhere else, so the screens work on a
  venue's network without the internet. They are served with `ETag` and `Cache-Control: no-cache`,
  so a new version of the service is picked up at once.
- **A strict content security policy**: `default-src 'none'` with only the service's own script, style,
  font, icon and connections allowed, no inline script or style, no frames. A customer's note appears on
  the kitchen's screen, so the code puts all data on the page as text (`textContent`), never as
  HTML; a test fails the build if the scripts use `innerHTML`, `eval` and the like, and another
  if the page has an inline script or style.
- **The look is that of a company's site**: a white bar at the top with the screens as its menu
  (the one in use underlined in orange), a navy banner with a title in tall capitals on the two
  pages that a customer lives on, menu cards with a picture, one orange accent, navy buttons with an
  orange one for the main action of a view, and a light and a dark scheme. An order is a card with a
  bar of the colour of its state; the kitchen's tickets are bigger, and a late one has a red bar.
  Headings are in Barlow Condensed, text in Be Vietnam Pro, which was made with Vietnamese in mind;
  the files, their sources and their licence (SIL OFL 1.1) are in
  [`web/fonts/README.md`](../../internal/orderservice/web/fonts/README.md). The picture of an item is
  a drawing by kind (drink, food, combo) until the menu carries a photo for it; the card then
  shows the photo, with no change to the page. Only a card that newly appears, a status label when
  the status changes, a toast, the live dot, a lifting card on hover and a late ticket's bar move,
  and none of it moves for people who ask the system for less motion. The colours are variables at
  the top of `web/app.css`: changing the look is changing that file.
- **Two languages, Vietnamese and English**, with a switch (VI | EN) in the top bar. The language is
  the person's choice, kept in their browser; before they choose, it is the browser's own language
  if that is Vietnamese, else English. The text in the code is English and is also the key
  (`T("Place order")`), so a sentence with no Vietnamese entry shows in English instead of
  breaking; `web/vi.js` is the dictionary and `web/i18n.js` the mechanism. Three tests keep it
  honest: every `T()` of the scripts has an entry and every entry is used (and both fill in the same
  `{names}`), and every fixed message of the accounts that the service writes is in the dictionary
  as written, read from the Go sources. Times follow the language. **Limits:** the messages of the
  order rules (such as "order: wrong state") are in English; the screen puts a sentence in
  Vietnamese before them. The names of items, categories of a venue's own and ingredients are the
  venue's data and show as written. The documentation stays in English.
- **Live without polling**: the screen reads the state, then listens to `GET /api/stream` (with
  `fetch`, because `EventSource` cannot send the token) and asks again for what changed. It
  reconnects with a growing pause, treats 45 seconds of silence as a dead connection, and refreshes
  every 20 seconds anyway. The pill in the header says whether it is live, reconnecting or off.
- **A command is never done twice**: every button makes a `command_id` and keeps it until the
  answer arrives, so pressing again after a timeout repeats the same command.
- **The logic is testable**: the rules of the screens that are not drawing (money formats, the
  option rules, the cart's body, which orders belong in which column, the refund policy) are in
  `web/logic.js`, covered by `node --test internal/orderservice/web/logic.test.js`. `go test`
  runs them too when `node` is installed, and skips them with a message when it is not.

What was checked in a real browser (Chromium in the desktop app): each screen above, with the
sample users, on a three-node cluster; a customer's order paid in another tab appearing in the
first without a reload; a note containing `<b>` shown as text; the refund above the total refused;
numbers continuing after a restart of the service; dark and light schemes; a phone-width viewport
with no horizontal scroll. It was one person clicking, not a test suite: there are no automated
browser tests yet.

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

Shifts and the cash drawer, reports, editing the menu and the promotions without a restart,
customers who register themselves and points (the accounts are made by an administrator for now),
several writers or read-only replicas, snapshots of the state, automated browser tests of the
screens, and a printed receipt or a display for customers waiting. A language other than English on
the screens is not done either: the text is in the files, in one place each. The menu, the
promotions and the stock recipes are still files read at start; the database holds only accounts.
