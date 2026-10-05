package proxy

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func (h *Handler) adminPageAuthenticated(r *http.Request) bool {
	// Asset requests do not accept password headers or consume login attempts.
	// HTTP-only sessions are the only browser authentication mechanism.
	cookie, err := r.Cookie(adminSessionCookie)
	return err == nil && h.validateAdminSession(cookie.Value)
}

func (h *Handler) serveAdminAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" || path.Clean(name) != name || strings.Contains(name, "\\") ||
		strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") {
		http.NotFound(w, r)
		return
	}
	public := name == "login.html" || name == "login.js" || name == "login.css"
	if !public && !h.adminPageAuthenticated(r) {
		http.NotFound(w, r)
		return
	}
	// Static assets are deployment-owned. Reject directories and symlinks escaping
	// the asset tree without requiring a newer Go version than the module.
	root, err := filepath.EvalSymlinks("web")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || !filepath.IsLocal(relative) {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}
