package dashboard

import "net/http"

func (d *Dashboard) handleErrors(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 8.
	d.render(w, r, "errors.html", "errors-cards", nil)
}

func (d *Dashboard) handleErrorDetail(w http.ResponseWriter, r *http.Request) {
	// Implemented in scope card 8.
	d.render(w, r, "error_detail.html", "", nil)
}
