package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Product images come from the server's /uploads/. Every image gets a new
// random name when it changes, so a copy on disk never goes stale: the agent
// keeps one, and the kiosk shows its pictures right after a reboot (Edge kiosk
// mode keeps no browser cache).

var (
	uploadDirPart  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	uploadFilePart = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}\.(png|jpg|jpeg|svg|webp)$`)
)

var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".svg": "image/svg+xml", ".webp": "image/webp",
}

func imageCacheDir(dataDir string) string { return filepath.Join(dataDir, "cache", "uploads") }

// validUpload accepts paths such as products/3f9c...e1.png and nothing that
// could leave the cache folder.
func validUpload(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || len(parts) > 3 {
		return false
	}
	for _, p := range parts[:len(parts)-1] {
		if !uploadDirPart.MatchString(p) {
			return false
		}
	}
	return uploadFilePart.MatchString(strings.ToLower(parts[len(parts)-1]))
}

func (a *Agent) handleUploads(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/uploads/")
	if !validUpload(rel) {
		http.NotFound(w, r)
		return
	}
	local := filepath.Join(a.imageDir, filepath.FromSlash(rel))
	if data, err := os.ReadFile(local); err == nil {
		serveImage(w, rel, data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	resp, err := a.srv.Get(ctx, "/uploads/"+rel)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable) // the page shows its "no image" tile
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err == nil {
		tmp := local + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			_ = os.Rename(tmp, local)
		}
	}
	serveImage(w, rel, data)
}

func serveImage(w http.ResponseWriter, rel string, data []byte) {
	h := w.Header()
	h.Set("Content-Type", imageTypes[strings.ToLower(filepath.Ext(rel))])
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	_, _ = w.Write(data)
}
