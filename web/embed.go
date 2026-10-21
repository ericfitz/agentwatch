// Package web holds the browser client the relay serves.
package web

import "embed"

//go:embed index.html app.js app.css codec.js static
var FS embed.FS
