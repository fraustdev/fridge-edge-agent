package main

import (
	_ "embed"
	"net/http"
)

//go:embed dashboard.html
var dashboardHTML []byte

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
