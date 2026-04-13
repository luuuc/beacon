package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
)

type errorCardData struct {
	Name          string
	Fingerprint   string
	FirstAppFrame string
	IsNew         bool
	Sparkline     template.HTML
	Count         int64
	LastSeen      string
}

func (d *Dashboard) handleErrors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get all errors.
	resp, err := d.reads.GetErrors(ctx, reads.GetErrorsRequest{})
	if err != nil {
		d.log.Error("errors query", "err", err)
	}

	// Also get new-only to tag NEW badges.
	newResp, _ := d.reads.GetErrors(ctx, reads.GetErrorsRequest{NewOnly: true})
	newFPs := map[string]bool{}
	if newResp != nil {
		for _, e := range newResp.Errors {
			newFPs[e.Fingerprint] = true
		}
	}

	var cards []errorCardData
	if resp != nil {
		for _, e := range resp.Errors {
			cards = append(cards, errorCardData{
				Name:        e.Name,
				Fingerprint: e.Fingerprint,
				IsNew:       newFPs[e.Fingerprint],
				Sparkline:   template.HTML(""),
				Count:       e.Occurrences,
				LastSeen:    formatTimeAgo(e.LastSeen),
			})
		}
	}

	d.render(w, r, "errors.html", "errors-cards", pageData(map[string]any{
		"ActiveNav": "errors",
		"Title":     "Errors",
		"Errors":    cards,
	}))
}

func (d *Dashboard) handleErrorDetail(w http.ResponseWriter, r *http.Request) {
	fingerprint := r.PathValue("fingerprint")
	ctx := r.Context()

	// Get error occurrences for this fingerprint by querying metrics.
	resp, err := d.reads.GetErrors(ctx, reads.GetErrorsRequest{})
	if err != nil {
		d.log.Error("error detail query", "fingerprint", fingerprint, "err", err)
	}

	var match *reads.ErrorSummary
	if resp != nil {
		for i, e := range resp.Errors {
			if e.Fingerprint == fingerprint {
				match = &resp.Errors[i]
				break
			}
		}
	}

	name := fingerprint
	if match != nil {
		name = match.Name
	}

	// Get hourly breakdown for the chart.
	metricResp, _ := d.reads.GetMetric(ctx, reads.GetMetricRequest{
		Kind:       beacondb.KindError,
		Name:       name,
		PeriodKind: "hour",
	})

	var chart template.HTML
	var stats []stat
	if metricResp != nil && len(metricResp.Data) > 0 {
		points := make([]ChartPoint, len(metricResp.Data))
		for i, pt := range metricResp.Data {
			points[i] = ChartPoint{Label: pt.PeriodStart, Value: float64(pt.Count)}
		}
		chart = ChartSVG(ChartOptions{
			Width: 800, Height: 200,
			Series: points,
		})
	}

	if match != nil {
		stats = append(stats, stat{"Exception", match.Name})
		stats = append(stats, stat{"Fingerprint", match.Fingerprint})
		stats = append(stats, stat{"First seen", match.FirstSeen})
		stats = append(stats, stat{"Last seen", match.LastSeen})
		stats = append(stats, stat{"Occurrences", fmt.Sprintf("%d", match.Occurrences)})
	}

	// Try to get a sample stack trace from raw events.
	var stackTrace string
	events, _ := d.reads.GetRecentErrorEvents(ctx, fingerprint, 1)
	if len(events) > 0 {
		if st, ok := events[0].Properties["stack_trace"].(string); ok {
			stackTrace = st
		}
	}

	d.render(w, r, "error_detail.html", "", pageData(map[string]any{
		"ActiveNav":   "errors",
		"Title":       name,
		"Name":        name,
		"Fingerprint": fingerprint,
		"Chart":       chart,
		"Stats":       stats,
		"StackTrace":  stackTrace,
	}))
}

func formatTimeAgo(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
