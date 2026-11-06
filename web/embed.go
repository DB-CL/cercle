// Package web embeds the interface page: HTML, CSS and JavaScript with no
// framework, no build step and no CDN.
package web

import "embed"

// FS holds index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var FS embed.FS
