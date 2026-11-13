# Fonts of the screens

Two typefaces, each as the `latin` and `vietnamese` subsets (the `latin-ext` subset is left out
because nothing here needs it), embedded in the program by `web.go` and served at `/fonts/`.

| Files | Typeface | Used for | Source |
|---|---|---|---|
| `barlow-600-*.woff2`, `barlow-700-*.woff2` | Barlow Condensed, weights 600 and 700 | headings, the banner, order numbers and figures | Google Fonts, `fonts.gstatic.com/s/barlowcondensed/v13` |
| `bevn-400-*.woff2`, `bevn-500-*.woff2`, `bevn-600-*.woff2`, `bevn-700-*.woff2` | Be Vietnam Pro, weights 400 to 700 | all the text | Google Fonts, `fonts.gstatic.com/s/bevietnampro/v12` |

Both are licensed under the SIL Open Font License 1.1, which allows embedding and redistributing
them with a program. Be Vietnam Pro was designed with Vietnamese in mind, which is why it is the
text face here.
The unicode ranges in `../app.css` are those of Google's stylesheet, so a browser fetches the
Vietnamese file only when a Vietnamese letter or `₫` is on the page. If a font file is missing
the screens fall back to the device's system fonts.
