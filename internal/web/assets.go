package web

import "embed"

// assetsFS embeds internal/web/assets verbatim: style.css (ported from
// ui-mockup/assets/style.css, its tokens remapped to the Trinetra palette),
// the stripped app.js (see that file's header comment for what was cut vs
// kept), the vendored htmx/uPlot static files, the Trinetra brand files
// (brand/: favicons and the ash/ink wordmarks, copied from the brand kit)
// and the Outfit display font (fonts/: OFL latin woff2 + OFL.txt). Nothing
// here is fetched from a CDN: routes.go serves all of it under /assets/
// straight out of the embedded filesystem. This package is only linked into
// trinetra-web; the core trinetra binary embeds none of it.
//
//go:embed assets
var assetsFS embed.FS
