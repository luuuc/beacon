package dashboard

import (
	"fmt"
	"html/template"
	"math"
	"net/http"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
)

type outcomeCardData struct {
	Name       string
	Sparkline  template.HTML
	Count      int64
	Drift      string
	DriftClass string
}

func (d *Dashboard) handleOutcomes(w http.ResponseWriter, r *http.Request) {
	summaries, err := d.reads.GetOutcomeSummaries(r.Context(), 0)
	if err != nil {
		d.log.Error("outcomes query", "err", err)
	}

	var cards []outcomeCardData
	for _, s := range summaries {
		drift, cls := driftLabel(s.DriftPercent)
		cards = append(cards, outcomeCardData{
			Name:       s.Name,
			Sparkline:  SparklineSVG(s.HourlyCounts, 200, 32),
			Count:      s.DailyCount,
			Drift:      drift,
			DriftClass: cls,
		})
	}

	d.render(w, r, "outcomes.html", "outcomes-cards", pageData(map[string]any{
		"ActiveNav": "outcomes",
		"Title":     "Outcomes",
		"Metrics":   cards,
	}))
}

func (d *Dashboard) handleOutcomeDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := r.Context()

	resp, err := d.reads.GetMetric(ctx, reads.GetMetricRequest{
		Kind:       beacondb.KindOutcome,
		Name:       name,
		PeriodKind: "day",
	})
	if err != nil {
		d.log.Error("outcome detail query", "name", name, "err", err)
		d.render(w, r, "metric_detail.html", "", pageData(map[string]any{
			"ActiveNav": "outcomes",
			"Title":     name,
			"Name":      name,
		}))
		return
	}

	// Build chart data.
	points := make([]ChartPoint, len(resp.Data))
	for i, pt := range resp.Data {
		points[i] = ChartPoint{Label: pt.PeriodStart, Value: float64(pt.Count)}
	}

	var baseline *float64
	if resp.Baseline != nil && resp.Baseline.HourlyCountMean > 0 {
		// Convert hourly mean to daily estimate for chart overlay.
		daily := resp.Baseline.HourlyCountMean * 24
		baseline = &daily
	}

	chart := ChartSVG(ChartOptions{
		Width: 800, Height: 250,
		Series:   points,
		Baseline: baseline,
	})

	// Stats.
	var stats []stat
	var totalCount int64
	for _, pt := range resp.Data {
		totalCount += pt.Count
	}
	stats = append(stats, stat{"Total (window)", fmt.Sprintf("%d", totalCount)})
	stats = append(stats, stat{"Period", resp.PeriodKind})
	if resp.Baseline != nil {
		stats = append(stats, stat{"Baseline mean (hourly)", fmt.Sprintf("%.1f", resp.Baseline.HourlyCountMean)})
		stats = append(stats, stat{"Baseline stddev", fmt.Sprintf("%.1f", resp.Baseline.HourlyCountStd)})
		stats = append(stats, stat{"Baseline captured", resp.Baseline.CapturedAt})
	}

	d.render(w, r, "metric_detail.html", "", pageData(map[string]any{
		"ActiveNav": "outcomes",
		"Title":     name,
		"Name":      name,
		"Chart":     chart,
		"Stats":     stats,
	}))
}

type stat struct {
	Label string
	Value string
}

func driftLabel(pct float64) (string, string) {
	abs := math.Abs(pct)
	switch {
	case abs < 1:
		return "flat", "drift-flat"
	case pct > 0:
		return fmt.Sprintf("↑ %.0f%%", pct), "drift-up"
	default:
		return fmt.Sprintf("↓ %.0f%%", abs), "drift-down"
	}
}
