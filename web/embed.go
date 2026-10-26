// Package web contains the plugin's self-contained browser GUI.
package web

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
