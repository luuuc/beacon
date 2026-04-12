package dashboard

import "net/http"

func (d *Dashboard) handlePerformance(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 7.
	d.render(w, r, "performance.html", "performance-cards", nil)
}

func (d *Dashboard) handlePerformanceDetail(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 7.
	d.render(w, r, "endpoint_detail.html", "", nil)
}
