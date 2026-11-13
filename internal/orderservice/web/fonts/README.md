# Fonts of the screens

Two typefaces, each as the `latin` and `vietnamese` subsets (the `latin-ext` subset is left out
because nothing here needs it), embedded in the program by `web.go` and served at `/fonts/`.

| Files | Typeface | Used for | Source |
|---|---|---|---|
| `bricolage-*.woff2` | Bricolage Grotesque (variable, weights 500 to 700) | names, headings, reading text | Google Fonts, `fonts.gstatic.com/s/bricolagegrotesque/v9` |
| `spacemono-400-*.woff2`, `spacemono-700-*.woff2` | Space Mono | numbers, money, labels, status stamps | Google Fonts, `fonts.gstatic.com/s/spacemono/v17` |

Both are licensed under the SIL Open Font License 1.1, which allows embedding and redistributing
them with a program. Bricolage Grotesque is by Mathieu Triay, Space Mono by Colophon Foundry.
The unicode ranges in `../app.css` are those of Google's stylesheet, so a browser fetches the
Vietnamese file only when a Vietnamese letter or `₫` is on the page. If a font file is missing
the screens fall back to the device's system fonts.
