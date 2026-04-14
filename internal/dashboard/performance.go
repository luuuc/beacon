package dashboard

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type perfCardData struct {
	Name        string
	Sparkline   template.HTML
	CurrentP95  string
	BaselineP95 string
	Drift       string
	DriftClass  string
	Volume      string
}

func (d *Dashboard) handlePerformance(w http.ResponseWriter, r *http.Request) {
	resp, err := d.reads.GetPerfEndpoints(r.Context(), reads.GetPerfRequest{})
	if err != nil {
		d.log.Error("performance query", "err", err)
	}

	p := message.NewPrinter(language.English)
	var cards []perfCardData
	if resp != nil {
		for _, ep := range resp.Endpoints {
			drift, cls := sigmaDriftLabel(ep.DriftSigmas)
			cards = append(cards, perfCardData{
				Name:        ep.Name,
				Sparkline:   template.HTML(""),
				CurrentP95:  fmt.Sprintf("%.0fms", ep.CurrentP95),
				BaselineP95: fmt.Sprintf("%.0fms", ep.BaselineP95),
				Drift:       drift,
				DriftClass:  cls,
				Volume:      p.Sprintf("%d", ep.RequestCount),
			})
		}
	}

	d.render(w, r, "performance.html", "performance-cards", pageData(map[string]any{
		"ActiveNav": "performance",
		"Title":     "Performance",
		"Endpoints": cards,
	}))
}

func (d *Dashboard) handlePerformanceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := r.Context()

	resp, err := d.reads.GetMetric(ctx, reads.GetMetricRequest{
		Kind:       beacondb.KindPerf,
		Name:       name,
		PeriodKind: "hour",
	})
	if err != nil {
		d.log.Error("perf detail query", "name", name, "err", err)
		d.render(w, r, "endpoint_detail.html", "", pageData(map[string]any{
			"ActiveNav": "performance",
			"Title":     name,
			"Name":      name,
		}))
		return
	}

	// Build chart from P95 values and collect volume data.
	var points []ChartPoint
	var p95Values []float64
	var totalRequests int64
	for _, pt := range resp.Data {
		val := 0.0
		if pt.P95 != nil {
			val = *pt.P95
		}
		points = append(points, ChartPoint{Label: pt.PeriodStart, Value: val})
		p95Values = append(p95Values, val)
		totalRequests += pt.Count
	}

	var baseline *float64
	if resp.Baseline != nil && resp.Baseline.HourlyCountMean > 0 {
		// For perf, baseline mean represents average hourly P95.
		baseline = &resp.Baseline.HourlyCountMean
	}

	chart := ChartSVG(ChartOptions{
		Width: 800, Height: 250,
		Series:   points,
		Baseline: baseline,
	})

	p := message.NewPrinter(language.English)
	var stats []stat
	if len(p95Values) > 0 {
		stats = append(stats, stat{"Current P95", fmt.Sprintf("%.0fms", mean(p95Values))})
	}
	stats = append(stats, stat{"Requests (7d)", p.Sprintf("%d", totalRequests)})
	if resp.Baseline != nil {
		stats = append(stats, stat{"Baseline mean (hourly)", fmt.Sprintf("%.1f", resp.Baseline.HourlyCountMean)})
		stats = append(stats, stat{"Baseline stddev", fmt.Sprintf("%.1f", resp.Baseline.HourlyCountStd)})
	}
	stats = append(stats, stat{"Window", "7d"})
	stats = append(stats, stat{"Period", resp.PeriodKind})

	d.render(w, r, "endpoint_detail.html", "", pageData(map[string]any{
		"ActiveNav": "performance",
		"Title":     name,
		"Name":      name,
		"Chart":     chart,
		"Stats":     stats,
	}))
}

func sigmaDriftLabel(sigmas float64) (string, string) {
	switch {
	case sigmas > 1:
		return fmt.Sprintf("%.1fσ slower", sigmas), "drift-up"
	case sigmas < -1:
		return fmt.Sprintf("%.1fσ faster", -sigmas), "drift-down"
	default:
		return "stable", "drift-flat"
	}
}

// mean computes the arithmetic mean (local helper to avoid exporting reads.mean).
func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
