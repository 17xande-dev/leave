package handler

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"time"
)

// Bundled assets: the stylesheet and a vendored htmx. See static/README.md for
// the vendoring record.
//
// Everything is served from this origin, which is why the CSP needs no
// allowance beyond 'self' for any of it. There is deliberately no STATIC_DIR
// override: this app serves one organisation's staff, not adopters who restyle
// without forking, and an override directory is a whole extra failure mode
// (missing dir, shadowed file, stale cache) for nobody's benefit.
//
//go:embed static
var staticFS embed.FS

// contentTypes maps an extension to what the file is served as.
//
// An explicit map rather than mime.TypeByExtension: an extension not listed here
// is not served at all, so a stray .html or .md under static/ cannot be published
// by accident. That is also why static/README.md is embedded but unreachable.
var contentTypes = map[string]string{
	".css":     "text/css; charset=utf-8",
	".js":      "text/javascript; charset=utf-8",
	".svg":     "image/svg+xml",
	".png":     "image/png",
	".ico":     "image/x-icon",
	".LICENSE": "text/plain; charset=utf-8",
}

// staticPrefix is where assets are served. It is not behind sign-in: the login
// page is rendered to an anonymous visitor and still needs its stylesheet.
// Assets are not secrets.
const staticPrefix = "/static/"

// staticAsset is one embedded file, with the URL it is served at.
type staticAsset struct {
	body        []byte
	contentType string
	etag        string
	url         string
}

// assets is built once at startup. A map rather than reading from the embed.FS per
// request so the content hash — which every rendered page needs for its asset URLs
// — is computed once.
var assets map[string]staticAsset

// loadAssets reads the embedded static directory. It returns an error only for a
// file whose extension is not in contentTypes, since that means someone added an
// asset the server has no intention of serving and the mismatch should surface at
// startup rather than as a 404 in a browser.
func loadAssets() (map[string]staticAsset, error) {
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("read embedded static: %w", err)
	}

	out := make(map[string]staticAsset, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		contentType, ok := contentTypes[path.Ext(name)]
		if !ok {
			// Embedded but not servable. README.md is the intended case; anything
			// else is worth knowing about, so skip quietly only for .md.
			if path.Ext(name) == ".md" {
				continue
			}
			return nil, fmt.Errorf("static asset %s has no content type; add its extension to contentTypes or remove the file", name)
		}
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			return nil, fmt.Errorf("read static/%s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])[:12]
		out[name] = staticAsset{
			body:        body,
			contentType: contentType,
			etag:        `"` + digest + `"`,
			url:         staticPrefix + name + "?v=" + digest,
		}
	}
	return out, nil
}

// assetURL resolves a bundled file to its content-addressed URL, for the {{asset}}
// template func. A name that does not exist becomes a visibly wrong URL rather
// than an empty attribute, so a typo shows up as a 404 in the network tab instead
// of a page that silently lost its stylesheet.
func assetURL(name string) string {
	a, ok := assets[name]
	if !ok {
		return staticPrefix + "missing/" + name
	}
	return a.url
}

// ServeStatic serves one bundled asset. The URL carries a hash of the file's own
// contents, which is the only thing that makes an immutable cache honest: change
// the file and every page references a different URL.
func (h *Handler) ServeStatic(w http.ResponseWriter, r *http.Request) {
	a, ok := assets[r.PathValue("file")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.contentType)
	w.Header().Set("ETag", a.etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// Zero mtime: conditional requests go through the ETag alone, since an
	// embedded file has no meaningful modification time.
	http.ServeContent(w, r, r.PathValue("file"), time.Time{}, bytes.NewReader(a.body))
}
