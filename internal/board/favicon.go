package board

// faviconSVG is the mark, served at /favicon.svg and linked from every page.
//
// It is a copy of assets/favicon.svg rather than an embed: go:embed cannot reach outside the package
// directory, and duplicating 600 bytes is cheaper than a build step that generates Go from assets.
// The copy is kept honest by TestTheServedFaviconMatchesTheAsset, which reads the real file.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">
  <title>The night shift manager</title>
  <!-- The mark at half scale, with a tighter corner radius: a 56/128 radius at 16px reads as a circle
       and the tile stops looking like a tile. -->
  <defs>
    <mask id="crescent-fav">
      <rect width="64" height="64" fill="#fff"/>
      <circle cx="40" cy="32" r="19" fill="#000"/>
    </mask>
  </defs>
  <rect width="64" height="64" rx="13" fill="#0F172A"/>
  <circle cx="32" cy="32" r="23" fill="#F5E9D0" mask="url(#crescent-fav)"/>
  <rect x="42" y="29" width="13" height="6" rx="3" fill="#F59E0B"/>
</svg>
`

// iconLink is the tag every page's <head> carries. Relative, so it works behind any prefix the
// operator's reverse proxy uses.
const iconLink = `<link rel="icon" type="image/svg+xml" href="/favicon.svg">`
