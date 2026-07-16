//go:build embedui

package web

import (
	"embed"
	"io/fs"
)

// dist is populated by scripts/build-web.sh (a copy of the repo-root web/dist
// build) and is gitignored. It only needs to exist when building with
// `-tags embedui`; the default build uses content_placeholder.go instead.
//
//go:embed all:dist
var distFS embed.FS

// content returns the real SPA build embedded under `-tags embedui`.
func content() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
