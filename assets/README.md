# Assets

The mark is a crescent and one amber bar: the night, and the one item in flight.

| File | Use it for |
|---|---|
| `logo.svg` | The default. A dark tile, for README headers, icons and anywhere the background is unknown. |
| `logo-mono.svg` | Light backgrounds, print, one-colour contexts. No tile, so the mark carries itself. |
| `favicon.svg` | 16–32px. Same mark at half scale with a tighter corner radius. |
| `logo-wordmark.svg` | Slides and one-off documents, **on light backgrounds** (its text is ink). Uses the system sans, so it renders as a different face on different machines; the README uses the mark plus HTML text instead. |

Palette: night `#0F172A`, moon `#F5E9D0`, shift `#F59E0B`.

Both marks are drawn with a mask — a circle with a circle taken out — rather than two arcs, because
arcs with mismatched radii leave one horn a hairline scratch at small sizes. Keep that if you edit
them.

To rasterise: `qlmanage -t -s 512 -o out assets/logo.svg` (macOS) or any SVG-to-PNG tool. The SVGs
have no external references and no text in `logo.svg`, `logo-mono.svg` or `favicon.svg`, so they
render identically everywhere.
