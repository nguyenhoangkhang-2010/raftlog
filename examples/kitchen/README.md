# Sample configuration for `orderservice`

These three files are **invented examples** for trying the service out: a small café with
drinks and bánh mì, two offers, and one person for each role. The names, prices and recipes
are not a real venue's, and the tokens in `users.json` are public (they are in this
repository), so they must never be used for a service that other people can reach.

A real venue writes its own files. The format is described in
[docs/kitchen/service.md](../../docs/kitchen/service.md); the service refuses a file with a key
it does not know, so a typo is an error and not a setting that does nothing.

| File | What it holds |
|---|---|
| `menu.json` | Items with options, recipes (in grams, millilitres or pieces), a happy-hour price, one combo, and the quantity at which each ingredient counts as running low |
| `promotions.json` | A code that gives 10% off a first order, and "buy two drinks, the third is free" |
| `users.json` | Who may call the service, with which role, and for kitchen workers, at which station |

The stock is not in these files: it starts empty and is filled by receiving goods through the
API, as a storekeeper would, so that it is part of the log like everything else.
