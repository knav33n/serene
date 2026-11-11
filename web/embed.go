// Package web embeds the built login page. dist is committed, so building serene
// doesn't need Node. Rebuild it with npm run build.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built page.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
