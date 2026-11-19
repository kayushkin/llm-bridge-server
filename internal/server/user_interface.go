package server

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// The front end this server serves itself, so an install needs no other web
// server: the standalone bridge-ui build (ui/bridge-ui/standalone), compiled
// into the binary by the ui package. It lives under /ui/ because its page
// addresses would otherwise collide with this server's own routes (/sessions
// is both a page and a route). The files are public: they are the same code as
// the repo, and the app signs in through the login routes once it has loaded.
const (
	userInterfacePathPrefix = "/ui/"
	// userInterfaceAssetsPrefix is where the build puts files named by a hash
	// of their content. Such a name never changes meaning, so a browser may
	// keep it for good; a name under it that is missing is a 404, not a page.
	userInterfaceAssetsPrefix = "assets/"
	userInterfaceIndexFile    = "index.html"
)

// handleUserInterface answers a file of the build when the path names one, and
// the app's index.html for every other path, since those are pages the app
// routes in the browser.
func (s *Server) handleUserInterface(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(s.userInterfaceBuild, userInterfaceIndexFile)
	if errors.Is(err, fs.ErrNotExist) {
		writeJSONError(w, http.StatusServiceUnavailable, "user_interface_not_built",
			"this server was compiled without its front end; run scripts/build-ui.sh, then build and install the server again")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "user_interface_unreadable", err.Error())
		return
	}

	name := strings.TrimPrefix(r.URL.Path, userInterfacePathPrefix)
	if name != "" && name != userInterfaceIndexFile && fs.ValidPath(name) {
		info, statErr := fs.Stat(s.userInterfaceBuild, name)
		switch {
		case statErr == nil && !info.IsDir():
			if strings.HasPrefix(name, userInterfaceAssetsPrefix) {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, s.userInterfaceBuild, name)
			return
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			writeJSONError(w, http.StatusInternalServerError, "user_interface_unreadable", statErr.Error())
			return
		case strings.HasPrefix(name, userInterfaceAssetsPrefix):
			http.NotFound(w, r)
			return
		}
	}

	// index.html names the hashed files of this build, so it must be asked for
	// again each time or a browser keeps loading the build before this one.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, userInterfaceIndexFile, time.Time{}, bytes.NewReader(index))
}

// handleRootRedirectsToUserInterface sends a browser that opened the server's
// bare address to the front end.
func (s *Server) handleRootRedirectsToUserInterface(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, userInterfacePathPrefix, http.StatusFound)
}
