# 15. The menu and the promotions in PostgreSQL, as numbered versions

Date: 2026-11-14

## Status
Accepted

## Context
Until now the menu, the stock thresholds and the promotions were JSON files read at start. Changing a
price meant editing a file on the machine and restarting the service, which a manager cannot do and
which interrupts every screen that is open. ADR 14 gave the service a database; the menu is the next
thing that belongs in it, and the first that people other than an administrator must be able to change.

The data has three properties that shaped the design. A menu is edited as a whole by a person who
sees all of it (a price depends on the others, a combo names items), not row by row from many places.
The service must never run on a menu that was not accepted: the checks that a file always went through
(a repeated id, a price below zero, a combo that names an item that is not there) must still hold.
And an order, once taken, must keep its names, prices and recipe whatever happens to the menu later.

## Decision
- **A document, in versions.** The menu and the promotions are each one JSON document, the same
  documents as the files, in the table `catalog_versions` with the key (kind, version). A save is the
  next version; nothing is updated. A trigger refuses `UPDATE`, `DELETE` and `TRUNCATE`, as for the
  trail of accounts, so the history of who changed which price is as safe as the orders are. The
  alternative, a table of items with their own rows, was rejected: the checks are over the whole
  document, a change of three prices is one save and one version, and there is one thing to restore.
- **The checks are the file's checks.** A save is read by `ParseMenu` or `ParsePromotions`, the code
  that reads the files, before anything is stored. A document that does not pass is answered
  `400 invalid_document` with the message of those checks, and changes nothing. At start the latest
  version is read the same way; if it does not pass, the service does not start and says which version,
  because running on a menu nobody accepted is worse than not running.
- **Two people editing do not overwrite each other.** A save carries the version it was made on
  (`base_version`) and is inserted only if that is still the latest (`INSERT ... SELECT ... WHERE`
  with the guard in the statement, and the primary key as the last resort). The loser gets `409 stale`
  and is told to read again. There is no merge: an editor shows what is in the database, the manager
  makes the change again on it. This was chosen over a lock ("someone is editing") because a lock
  held by a browser tab that was closed is a worse failure than a repeated edit.
- **A saved version is in use at once, without a restart.** The service builds the new menu, then
  swaps it under the same write lock that commands take, so a command sees the old menu or the new one,
  never half. An order keeps what it was sold as: the lines of an order carry their names, unit prices
  and what they need, so cancelling after a change still returns the ingredients of the recipe the order
  was made with (a test takes and pays an order, changes the menu, cancels the order, and rebuilds the
  service from the log on the new menu). Every open screen is told with a `menu` event on the stream
  and reads the menu again.
- **Putting an old version back makes a new one.** The history is never rewritten: "restore 1" saves
  the text of version 1 as version 4, after being checked again, because the rules may have changed.
- **Ingredients are not a table.** A recipe still names an ingredient by a string, and an ingredient
  exists when a recipe uses it or the stock has it. A new ingredient is therefore sold out until the
  storekeeper receives it, and the storekeeper may receive it as soon as a saved menu uses it. The low-stock
  thresholds are part of the menu document; saving a menu replaces them, so a threshold removed is gone.
- **Photos are rows, outside the versions.** `item_images` keeps one JPEG or PNG per item as `bytea`,
  at most 1 MiB, with sides of 64 to 4096 pixels. The type is read from the bytes (`image.DecodeConfig`),
  never from the name or the header the browser sent, so an SVG with a script, which a browser would run,
  is not accepted. The photo is served, to anyone like the menu, at an address that has the first 12
  characters of its SHA-256 (`/api/images/{id}?v=...`) with `Cache-Control: immutable`, an `ETag`,
  `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src 'none'; sandbox`. A new
  photo is a new address, so no cache holds an old one. A photo does not make a version of the menu: it
  is not part of the checks, and a restore does not move it.
- **Who may.** `menu.manage` and `promo.manage`, which the manager has, as the permissions already said;
  a cashier or a customer gets `403`, and a person who is not signed in `401`.
- **One migrator, one history table per set.** `pgmigrate` gets `WithTable`, so the accounts and the
  catalog each number their files from `0001` and keep their record in their own table
  (`schema_migrations`, `catalog_migrations`), and neither has to know the other's numbers.
- **The files become the first fill.** With `-database-url`, the menu file is read only when the
  database has no menu yet, and then saved as version 1 by `system`; after that the file is ignored (the
  log says so). Without a database, the token mode, the files are read at start as they always were.

## Consequences
- **The editor is a form for the common work and the whole text for the rest.** Items (price, category,
  station, recipe, option groups, prices by time), combos, thresholds and promotions have forms; every
  field of the document can be changed in a text view, which is checked the same way. The forms build the
  document and the tests (`web/admin.test.js`) run those conversions: a document opened and saved without
  a change comes out as it went in, and what the form lets through is what the service reads.
- **Two copies of the checks are not kept.** The form checks only what a person can fix at once (a
  number is a number); everything else is the service's answer, in English, behind a sentence in the
  person's language. The fixed messages of the catalog are in the Vietnamese dictionary and a test reads
  them from the Go sources.
- **The history shows the login name that saved, and a note that the program wrote** (such as
  `edited item coffee`, in English). A manager cannot type a note in the forms yet.
- **A photo is in the database, so the database is bigger and its backups too**: a menu of fifty items at
  the limit is 50 MiB. It is simple and keeps one thing to back up; files on disk or object storage would
  be the next step if the photos grow, and the address of a photo would not change.
- **One instance only is assumed to be running for a swap to be seen at once.** The `menu` event is local
  to the process that saved. With two instances, the other would keep the old menu until it restarts; a
  poll of the latest version, or PostgreSQL `LISTEN/NOTIFY`, would fix that and is not done.
- **The swap is in memory after the insert, not in the same transaction.** If the process dies between
  the insert and the swap, the manager was not told "saved", and the next start reads that version
  anyway: the database is the truth. If the swap itself failed (it should not, the document was
  checked) the answer is an error and the service keeps the old menu until the next start.
- **Not done:** a note typed by the manager, comparing two versions (only showing one), scheduling a
  version for a later time, a recycle bin for photos, ingredients with units and names of their own.
