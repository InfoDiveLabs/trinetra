package web

import "embed"

// assetsFS embeds internal/web/assets verbatim: the mockup's style.css
// (copied byte-for-byte from ui-mockup/assets/style.css — it IS the design
// system, not restyled here), the stripped app.js (see that file's header
// comment for what was cut vs kept), and the vendored htmx/uPlot static
// files. Nothing here is fetched from a CDN: routes.go serves all of it
// under /assets/ straight out of the embedded filesystem.
//
//go:embed assets
var assetsFS embed.FS
