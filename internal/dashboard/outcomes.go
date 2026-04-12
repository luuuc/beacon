package dashboard

import "net/http"

func (d *Dashboard) handleOutcomes(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 6.
	d.render(w, r, "outcomes.html", "outcomes-cards", nil)
}

func (d *Dashboard) handleOutcomeDetail(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 6.
	d.render(w, r, "metric_detail.html", "", nil)
}
