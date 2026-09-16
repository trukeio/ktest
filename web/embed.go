// Package web carries ktestd's rack: the browser UI, built.
//
// dist/ is committed on purpose. The Go toolchain will not run a bundler, so
// embedding a build output means the output has to be in the repository for
// ktestd to build without a Node toolchain. Rebuild it with
// `npm install && npm run build` in this directory after changing anything
// under src/.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// The all: prefix matters. Without it embed silently skips files whose names
// begin with a dot or an underscore, which shows up as a blank page rather
// than as a build error.
//
//go:embed all:dist
var files embed.FS

// Handler serves the built UI.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		// Only reachable if the embedded tree is missing, which is a build
		// error rather than a runtime condition.
		panic("web: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
