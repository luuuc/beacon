package dashboard

import "net/http"

func (d *Dashboard) handleLanding(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 4.
	d.render(w, r, "landing.html", "", nil)
}
