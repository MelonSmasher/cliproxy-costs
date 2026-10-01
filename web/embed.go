// Package web embeds the dashboard assets.
package web

import "embed"

// Files holds the dashboard sources and the vendored chart library.
//
//go:embed index.html app.js app.css vendor/uPlot.iife.min.js vendor/uPlot.min.css
var Files embed.FS
