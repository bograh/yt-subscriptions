// Package static embeds the browser assets so the binary is self-contained.
//
// app.css is generated from styles/input.css (make css); htmx.min.js and the
// fonts are vendored. app.js is hand-written.
package static

import "embed"

// FS holds the files served under /static/.
//
//go:embed app.css app.js htmx.min.js fonts
var FS embed.FS
