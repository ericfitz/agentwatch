// Package web holds the browser client the relay serves.
package web

import "embed"

//go:embed codec.js static
var FS embed.FS
