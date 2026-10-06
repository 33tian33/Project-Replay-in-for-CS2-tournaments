package replay

import (
	"io"
	"io/fs"
	"net/http"
)

// Ship the supplied Astra assets with Replay; isolate their scripts from the
// control API exactly as for imported HUD packages.
func (a *Service) astraRoutes(mux *http.ServeMux) {
	assets, _ := fs.Sub(webFiles, "web/astra")
	files := http.StripPrefix("/astra/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /astra/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'self' data: blob: http: https:; script-src 'self' 'unsafe-inline' http: https:; style-src 'self' 'unsafe-inline' http: https:; connect-src 'self' http: https:; object-src 'none'")
		if r.URL.Path == "/astra/replay-bridge.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			io.WriteString(w, hudBridge)
			return
		}
		if r.URL.Path == "/astra/index.html" {
			html, err := fs.ReadFile(assets, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(html)
			return
		}
		files.ServeHTTP(w, r)
	})
}
