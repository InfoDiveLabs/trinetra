package web

import "embed"

// assetsFS embeds the UI assets; nothing is fetched from a CDN.
//
//go:embed assets
var assetsFS embed.FS
