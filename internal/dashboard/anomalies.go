package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
)

type anomalyCardData struct {
	ID           int64
	AnomalyKind  string
	BadgeClass   string // CSS class: "shift" or "spike"
	Name         string
	Dimension    string // formatted dimension string, empty if none
	Current      int64
	BaselineMean string
	Sigma        string
	Summary      string
	Link         string
	FirstDetected string
}

func (d *Dashboard) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp, err := d.reads.GetAnomalies(ctx, reads.GetAnomaliesRequest{Since: 7 * 24 * time.Hour})
	if err != nil {
		d.log.Error("anomalies query", "err", err)
	}

	var cards []anomalyCardData
	if resp != nil {
		for _, a := range resp.Anomalies {
			cards = append(cards, toAnomalyCard(a))
		}
	}

	d.render(w, r, "anomalies.html", "anomalies-cards", pageData(map[string]any{
		"ActiveNav": "dashboard",
		"Title":     "Anomalies",
		"Anomalies": cards,
	}))
}

func toAnomalyCard(a reads.AnomalyEntry) anomalyCardData {
	badge := "shift"
	if a.AnomalyKind == "dimension_spike" {
		badge = "spike"
	}

	var dimStr string
	if len(a.Dimension) > 0 {
		keys := make([]string, 0, len(a.Dimension))
		for k := range a.Dimension {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, a.Dimension[k]))
		}
		dimStr = strings.Join(parts, ", ")
	}

	link := linkForAnomaly(a)

	return anomalyCardData{
		ID:           a.ID,
		AnomalyKind:  a.AnomalyKind,
		BadgeClass:   badge,
		Name:         a.Name,
		Dimension:    dimStr,
		Current:      a.Current,
		BaselineMean: fmt.Sprintf("%.0f", a.BaselineMean),
		Sigma:        fmt.Sprintf("%.1fσ", a.DeviationSigma),
		Summary:      a.Summary,
		Link:         link,
		FirstDetected: a.FirstDetected,
	}
}

func (d *Dashboard) handleDismissAnomaly(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := d.reads.DismissAnomaly(r.Context(), id); err != nil {
		if errors.Is(err, beacondb.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		d.log.Error("dismiss anomaly", "err", err, "id", id)
		http.Error(w, "dismiss failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkForAnomaly returns a link to the relevant pillar detail page.
func linkForAnomaly(a reads.AnomalyEntry) string {
	switch a.MetricKind {
	case "perf":
		return "/performance/" + url.PathEscape(a.Name)
	case "error":
		return "/errors"
	case "outcome":
		return "/outcomes/" + url.PathEscape(a.Name)
	default:
		return ""
	}
}
