package main

import (
	_ "embed"
	"net/http"
)

//go:embed dashboard.html
var dashboardHTML []byte

//go:embed leaflet.js
var leafletJS []byte

//go:embed leaflet.css
var leafletCSS []byte

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
