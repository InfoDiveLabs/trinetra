package web

import "embed"

// assetsFS embeds internal/web/assets verbatim: style.css (tokens mapped to
// the Trinetra palette), app.js, the vendored htmx/uPlot static files, the
// Trinetra brand files (brand/: favicons and the ash/ink wordmarks) and the
// Outfit display font (fonts/: OFL latin woff2 + OFL.txt). Nothing here is
// fetched from a CDN: routes.go serves all of it under /assets/ straight out of
// the embedded filesystem. This package is only linked into trinetra-web; the
// core trinetra binary embeds none of it.
//
//go:embed assets
var assetsFS embed.FS
