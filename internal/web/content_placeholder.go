//go:build !embedui

package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist-placeholder
var placeholderFS embed.FS

// content returns the placeholder SPA embedded in the default build. Build with
// `-tags embedui` (via scripts/build-web.sh) to embed the real UI instead.
func content() fs.FS {
	sub, err := fs.Sub(placeholderFS, "dist-placeholder")
	if err != nil {
		panic(err)
	}
	return sub
}
