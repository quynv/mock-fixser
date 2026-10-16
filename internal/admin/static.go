package admin

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const notBuilt = `<!doctype html><meta charset="utf-8"><title>mockfix</title>
<body style="font-family:system-ui;margin:2rem">
<h1>mockfix</h1><p>The Web UI is not built into this binary. Run <code>make web</code>, then rebuild.</p>
<p>The REST API is under <a href="/api/config">/api</a>; metrics at <a href="/metrics">/metrics</a>.</p></body>`

// spa serves the built UI. Unknown paths get index.html so client-side
// routes work; hashed assets are cached for a long time.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if f, err := ui.Open(name); err == nil {
				f.Close()
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		index, err := ui.Open("index.html")
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, notBuilt)
			return
		}
		defer index.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.Copy(w, index)
	})
}
