// Package web holds the embedded mobile-first frontend served by the API.
package web

import "embed"

//go:embed index.html app.js styles.css
var Files embed.FS
