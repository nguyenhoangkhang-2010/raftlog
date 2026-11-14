# 17. The call board: a public screen that shows numbers and nothing else

Date: 2026-11-14

## Status
Accepted

## Context
A customer who has paid waits somewhere in the shop, and the staff call out numbers or point at the
counter. A screen on the wall that shows which orders are ready ends the shouting. The screens of this
system are all behind a sign-in, with roles read at every request; a television on a wall is not a person
who signs in, and a session cookie on a screen that nobody watches over is a credential left in the open.

## Decision
- **The board is its own page, `/board`,** with its own script. It does not share the single page of the
  staff, so a screen on a wall carries none of their code, their data or their sessions.
- **What it may say is decided by what it carries.** `GET /api/board` answers with two lists, the orders
  being made and the orders that are ready, each entry a number and a time. There is no field for a name,
  an item or a total, so there is nothing to leak by mistake; a test checks that no other field appears
  and that no name or item of a test order is in the answer.
- **Public is a choice of the venue, and off by default.** With `-public-board` anyone who can reach the
  service reads the board without signing in, and the service says so in its log at start. Without it, the
  board is for people who are signed in and may see all orders (a cook, a cashier, a manager can put it on
  their own screen). The reason for a flag and not a role is that a screen cannot hold a password worth
  having; the reason it is off is that an open endpoint should be something somebody chose.
- **It is told, not asked, but it also asks.** The page reads the board, listens to a stream that says
  "board" whenever an order changes, and reads the board again; it also reads it every 20 seconds, so an
  event lost on a bad network is made good within that time, and after every reconnect. The stream carries
  no data of the order, only the fact: the board is the same answer whoever asks.
- **Bounded.** At most 100 streams are open at once (a public stream could otherwise be used to hold
  connections), a list has at most 60 numbers, and a private stream asks again at every keep-alive who is
  watching, as the staff's stream does, so that a person who was signed out stops watching.
- **A number that becomes ready is called.** It pulses for a few seconds and, if the person at the screen has
  switched the sound on, a short chime plays. A screen that has just been switched on calls nothing: it
  would otherwise call every waiting order at once. Sound is a switch because a browser lets a page make
  sound only after a click.
- **The runner has a screen of its own.** The till is for taking payment; a person who only carries food does
  not need its columns. The runner screen lists the ready orders oldest first as large cards (red after five
  minutes) with a "Hand over" button, and what is still being made. A cashier who also hands over has both.

## Consequences
- **The board shows order numbers to everyone in view of it.** That is its purpose; it is why it carries
  nothing else. A customer who photographs it learns that order 17 exists.
- **A public board is a public endpoint.** It is read-only and bounded, and it can be used to know how busy
  the venue is (the count of numbers). Behind a reverse proxy it should be given the same limits as the rest.
- **The first reading after the screen is switched on makes no sound and no pulse**, so a screen that was
  restarted in the middle of the day does not announce the room.
- **The time of the clock on the board is the screen's own**, not the service's; a wall screen with a wrong
  clock shows a wrong time, and nothing else.
- **Not done:** calling a number by voice, a board for several counters or stations, the name of a customer
  (a venue that wants it can ask; it would need a decision about whose name is shown), and a page of the
  board that works with the token mode of the users file without `-public-board`.
