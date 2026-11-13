# Kitchen: orders, kitchen stations and the till

The second application on raftlog, and the larger one: a system for a food stall, a
canteen or a café, with a customer ordering from their phone, a cashier at the till,
kitchen stations (drinks, hot, cold) each with a screen, runners, a manager and a
storekeeper. Its orders are a stream of events, which is what a replicated log is for.

This page describes the **domain core** (`internal/kitchen/...`): the rules, with no
database, network or screen. The service that puts them on raftlog is described in
[service.md](service.md), which also describes the screens that it serves to a browser.

## The packages

| Package | What it decides |
|---|---|
| `money` | Đồng as whole numbers, percentages in basis points, exact splitting of an amount over lines |
| `rbac` | Seven roles and what each may do ([the table](roles.md)) |
| `menu` | Items with options, combos, time-of-day prices; checks a customer's choice and freezes it into a priced line |
| `promo` | Percentage, fixed and buy-x-get-y offers, with codes, limits and stacking rules |
| `stock` | Ingredients as a count that never goes below zero, changed only by movements |
| `order` | The life of an order as events, who may cause each, and how it is rebuilt |

## The life of an order

```
                       pay                 start           finish (all stations)    hand over
   placed ──────────────► paid ──────────► preparing ──────────────► ready ──────────────► completed
     │                      │                  │                        │                      │
     │ cancel (own order    └── request cancel, then a manager decides ─┘                      └─ refund
     │  or cashier)                  │                                                           (manager)
     ▼                               ▼
  cancelled ◄────────────────── cancelled (with the refund the manager set)
```

Every arrow is an event. The order is those events, in order, and nothing else.

| Event | Who may cause it | Rules |
|---|---|---|
| `order.placed` | customer (own order, from the app), cashier (at the counter) | Lines, discounts and totals must add up; prices are frozen |
| `order.paid` | cashier | Pays the whole total. Cash must cover it (the change is computed); card and transfer need a reference |
| `ticket.started` / `ticket.ready` | kitchen worker **at that station** | Only after payment; not while a cancellation waits; one ticket per station the order needs |
| `order.handed_over` | cashier or runner | Only when every station is done; not while a cancellation waits |
| `cancel.requested` | customer (own order), cashier, manager | Needs a reason; only once paid; one request at a time |
| `cancel.rejected` | manager | The order carries on |
| `order.cancelled` | the person (unpaid), or the manager who approves a request | **Nobody approves their own request.** Before any station started the whole amount goes back and the ingredients return to the stock; afterwards the manager sets the refund (0 up to the total) and the ingredients stay used |
| `order.refunded` | manager | Only for a completed order, partial or whole, never more than the total in all |

Repeating a command (the log delivers at least once) is recognised by its ID and
changes nothing, and that is checked before anything else, so a resend of a command
that worked is not reported as forbidden.

## The rules that are easy to get wrong

- **Money is an integer.** A line is at most 99 units of at most 100 million đồng, an
  order at most 100 lines: no total gets near overflow, and the bounds are checked.
- **An order is a snapshot.** Prices, option names, the pieces each station makes and
  the ingredients used are copied into the order when it is taken. Editing the menu
  later changes no order.
- **A discount never makes a line or an order negative**, and it is spread over the
  lines so that they add up exactly to the order (largest remainder), which keeps
  refunds per line honest.
- **Promotions give the same result in any order.** Stackable ones apply by priority,
  each on what the earlier ones left; an exclusive one applies alone, and the customer
  gets whichever is better. A tie goes to the stackable ones.
- **Stock is taken at payment**, as one all-or-nothing movement that reports every
  ingredient that is short. It goes back only if the order is cancelled before work
  began. An item whose base recipe cannot be made once more is sold out and cannot be
  put in a cart.
- **Separation of duties.** The cashier takes money but cannot approve a cancellation
  or a refund; the manager approves but does not take payments; the administrator
  manages accounts and reads the audit log and can touch nothing else.

## What the tests show

| Claim | How it is tested |
|---|---|
| Each rule above | One test per rule, with the refusal and its reason |
| A command that is accepted is always accepted by `Apply` | 600 random lives of orders, with random people trying random commands, half of them guided to get deep |
| The invariants hold at every step (refunds never exceed the total, nothing starts on an unpaid order, ready means every station is done, ...) | Checked after every step of those lives |
| Replaying the events gives exactly the same order, also through JSON, and delivering every event twice changes nothing | At the end of every random life |
| A promotion result is the same whatever the order of lines and promotions, and always adds up | 1500 random orders |
| Stock never goes below zero and replay is exact | 300 random sequences of movements |
| The tests would notice a bug | 24 deliberate bugs (a rounding change, a reversed priority, self-approval allowed, an unchecked total, a repeated command not recognised, ...) were put into the code one at a time, and each was caught |

## Not here yet

The service is done in a reduced form ([service.md](service.md)): events in one topic, roles
enforced at the API, the menu, promotions and users read from files, and the screens for the
customer, the till, the kitchen, the manager and the storekeeper, served by the service itself.
Not done: automated tests that drive the screens in a browser (they were checked by hand), a
display for customers who are waiting, and printed receipts. Left out on purpose: a database,
accounts and login, shifts, and editing the menu without a restart.

The menu used in the tests, and the sample files in `examples/kitchen`, are invented. Real
menus are files that the venue provides.

One rule of this core was found missing by the service's tests: the table says that a ticket cannot
be started or finished while a cancellation waits for the manager, and finishing was not checked,
neither in the command nor in `Apply`. Both refuse it now, with tests (`order/pending_test.go`),
and the `Apply` check for starting, which had none, has one.
