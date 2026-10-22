// Package web embeds the compiled static assets and Go templates so the binary
// is fully self-contained.
package web

import "embed"

// Static holds the CSS, JS and font files served under /static/.
//
//go:embed static
var Static embed.FS

// Templates holds the html/template sources.
//
//go:embed templates
var Templates embed.FS
