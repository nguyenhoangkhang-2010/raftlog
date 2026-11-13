# 14. Accounts in PostgreSQL, beside the log

Date: 2026-11-13

## Status
Accepted

## Context
ADR 13 kept the kitchen service stateless: orders and stock live in one raftlog topic, and the
people who may use it are a file of fixed tokens. That is enough to make the roles real and not
enough for a place where people come and go: a token never expires, cannot be taken back without
editing a file and restarting, tells nothing about who did what, and cannot be given to a new
cook by anyone but whoever holds the file.

Accounts, and soon the menu, the shifts, the reports and customers' points, are data of a different
kind from the orders. They are read and written in many small pieces, looked up by name, joined,
counted and changed in place; the orders are a sequence of facts that are only ever added. A
replicated log is the right home for the second and a poor one for the first. So the system gets a
database for the first, and keeps the log for the second.

## Decision
- **PostgreSQL holds the accounts; the log still holds every order, payment and stock movement.**
  The service talks to both. An order carries the *login name* of whoever touched it, as a string,
  and an account is disabled, never deleted or renamed, so a name in the log never points at nothing.
- **A login name is the identity.** 3 to 32 characters, lower case, fixed for life. It is the actor
  ID of the order rules, so nothing in the core changed.
- **Passwords are argon2id hashes** (64 MiB, 3 passes, 2 lanes, a random salt, in the PHC string
  format so a hash says how to check itself, and is renewed at sign-in when the settings get
  stronger). A policy of ten characters, not the login name, not one of the most used, not a few
  characters repeated. Hashing is limited to four at a time, because each takes 64 MiB and a burst
  of sign-ins must not exhaust the machine. An unknown name, a wrong password and a disabled account
  answer alike and cost alike (a hash is computed for a name that does not exist).
- **No email, so no "forgot password" link.** An administrator resets a password: the service makes
  a random one of 16 characters, shows it once and stores only its hash; the person must change it
  at the first sign-in, and until then the session opens nothing but the page that changes it. If the
  only administrator is locked out, `orderservice user set-password` does it from the command line.
- **Sessions are on the server.** A session is 256 random bits; the database keeps a hash of it. The
  browser holds it in a cookie that is `HttpOnly` (the page's script cannot read it, so a script
  injected into the page cannot take it), `SameSite=Strict` and, behind a proxy that does TLS,
  `Secure`. It ends after 12 hours without use and at the latest 7 days after sign-in, and is revoked
  at once when the account is disabled or its password is reset or changed, or the person asks. A
  server-side session was chosen over a signed token (JWT) because taking one back is the point.
- **Roles are read at every request**, never copied into the session. Taking a role away or
  disabling an account counts on the next request; an open stream is asked again every keep-alive
  (15 seconds) and is closed with `reset` when the person is gone or their roles changed. That check
  does not count as use, so a screen left open does not keep its session from going idle.
- **A request made by another site is refused.** `SameSite=Strict` is the first line. The second:
  a request that changes something must carry `Sec-Fetch-Site: same-origin` (or `none`), or, from a
  browser that sends no such header, an `Origin` that is this host. The sign-in itself is covered
  too, or a page of another site could sign a person in as itself.
- **Limits against guessing.** Five wrong passwords in a row lock the account for 15 minutes (the
  right password does not open it during the lock; an administrator can unlock). Each address may
  make 30 sign-in attempts a minute. The count of failures is kept under a lock on the account's row,
  so twenty simultaneous guesses are five failures and a lock, not twenty failures that each saw zero.
- **The last administrator stays, and nobody locks themselves out.** The last active administrator
  cannot be disabled or lose the role; nobody changes their own roles, disables or resets their own
  account. To make this true under concurrency, every change to an account first locks the account
  and all administrators in the order of their ids: without it, two administrators who disable each
  other at once each see another one left, or deadlock. A test repeats that race and fails within a
  few rounds when the ordering is taken out.
- **Everything that matters is written to `account_events`**, in the same transaction as the change:
  sign-ins that worked and did not, locks, password changes, who created, changed, disabled, unlocked
  or reset whom, sessions ended. A trigger refuses `UPDATE`, `DELETE` and `TRUNCATE` on it.
- **The schema is migrated by the program.** SQL files in `internal/accounts/migrations`, embedded,
  applied once each in order, each in a transaction, under an advisory lock so that instances starting
  together take turns. The checksum of each file is recorded: a file edited after it was applied is
  an error, and so is a database newer than the program. There is no "down": a mistake is fixed by
  a new migration.
- **The token file stays**, as a second way to sign in, for the tests (including the chaos test, which
  starts real processes) and for trying the service without a database. The two cannot be used
  together; with `-database-url`, bearer tokens are not accepted anywhere.

## Consequences
- **PostgreSQL is now something the service cannot do without.** If it does not answer, nobody can
  sign in and every request of a signed-in person answers `503` (not "wrong password"), and `/healthz`
  says so. The orders are safe in the log and the menu, which needs no account, is still served.
  Caching sessions for a few seconds would hide short outages and would delay revocation by as much;
  it is not done.
- **Two systems, no shared transaction.** Nothing here needs one: an account change and an order
  never have to commit together. The later parts will: a customer who pays with points changes the
  log and the database. That is the hard part of the plan, and will be designed when it comes.
- **The limit per address counts the address of the connection.** `X-Forwarded-For` is not believed
  (anyone who reaches the service could write it), so behind a reverse proxy every person has the
  proxy's address and shares one budget of 30 attempts a minute. That is enough for a shift starting
  together and is a real limit on a busy place; the fix is to name the proxies that may be trusted,
  and is not done. The limit is in memory, so several instances each count alone.
- **Locking can be used against the person it protects.** Someone who knows a login name can lock
  that account by guessing wrongly, and the address limit slows but does not stop them. This is the
  usual price; the administrator can unlock.
- **The trail is append-only for the program, not for the owner of the database.** Whoever owns the
  tables can drop the trigger. It guards against a bug and a careless statement, not against an
  administrator of the database; that would need the trail written elsewhere, as the orders are.
- **Not done:** TLS in the program (use a proxy), a second factor, a check of passwords against known
  breaches (only a short list of the most used), password reset by email, sessions listed with a
  place instead of an address, limits shared between instances.
- Three dependencies were added: `pgx` for PostgreSQL, `x/crypto` for argon2 and `x/term` to ask for a
  password without echoing it. They are pinned to versions that build with Go 1.22, which CI uses.
