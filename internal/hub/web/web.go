// Package web serves the built front end, embedded into the Hub binary
// (docs/M8 第 7 节). `npm run build` in web/ writes into dist/; without a build
// only .gitkeep is there and every page says so.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the application. Files under /assets/ are content-hashed by
// the build and cached for a year; everything else is the application shell
// (index.html), which is never cached so a new version shows up at once.
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	files := http.FS(sub)
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		index = []byte(`<!doctype html><meta charset="utf-8"><title>termhub</title><p style="font-family:sans-serif;margin:3em">termhub 已启动，但这个版本没有内嵌网页前端。请在 web/ 里执行 npm run build 后重新构建 Hub。</p>`)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" && p != "index.html" {
			if f, err := sub.Open(p); err == nil {
				st, _ := f.Stat()
				f.Close()
				if st != nil && !st.IsDir() {
					if strings.HasSuffix(p, ".webmanifest") {
						w.Header().Set("Content-Type", "application/manifest+json")
					}
					if strings.HasPrefix(p, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "no-cache")
					}
					http.StripPrefix("/", http.FileServer(files)).ServeHTTP(w, r)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(index)
	})
}
