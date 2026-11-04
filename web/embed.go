// Package web embarque la page de l'interface : HTML, CSS et JavaScript sans
// framework, sans étape de build, sans CDN.
package web

import "embed"

// FS contient index.html, app.js et style.css.
//
//go:embed index.html app.js style.css
var FS embed.FS
