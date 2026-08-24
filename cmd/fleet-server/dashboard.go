package main

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed dashboard.html
var dashboardHTML []byte

//go:embed leaflet.js
var leafletJS []byte

//go:embed leaflet.css
var leafletCSS []byte

//go:embed fonts
var fontsFS embed.FS

// dashboardHandler serves the read-only ops dashboard at "/". It's a plain
// static page that calls the same JSON endpoints everything else uses —
// no build step, no separate frontend project.
func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

// Leaflet (BSD-2-Clause, leafletjs.com) is vendored here rather than pulled
// from a CDN at page-load time, so the dashboard doesn't depend on a third
// party being up. Map tiles themselves still come from OpenStreetMap over
// the network -- that dependency can't be vendored away.
func leafletJSHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/javascript; charset=utf-8")
	w.Write(leafletJS)
}

func leafletCSSHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/css; charset=utf-8")
	w.Write(leafletCSS)
}

// Space Grotesk, Inter, and IBM Plex Mono (all SIL Open Font License) are
// vendored here as static woff2 files rather than pulled from Google Fonts
// at page-load time, for the same reason as Leaflet above -- no third-party
// dependency for the dashboard to render correctly.
func fontHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/fonts/")
	data, err := fontsFS.ReadFile("fonts/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "font/woff2")
	w.Write(data)
}
