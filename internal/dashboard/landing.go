package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"time"

	"github.com/luuuc/beacon/internal/reads"
)

// landingCard is the template data for one of the three headline cards.
type landingCard struct {
	Label     string
	Headline  string
	Sparkline template.HTML
	Detail    string
	Link      string
}

func (d *Dashboard) handleLanding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var cards []landingCard

	if card := d.outcomesHeadline(ctx); card != nil {
		cards = append(cards, *card)
	}
	if card := d.perfHeadline(ctx); card != nil {
		cards = append(cards, *card)
	}
	if card := d.errorsHeadline(ctx); card != nil {
		cards = append(cards, *card)
	}
	if card := d.anomaliesHeadline(ctx); card != nil {
		cards = append(cards, *card)
	}

	d.render(w, r, "landing.html", "", pageData(map[string]any{
		"ActiveNav": "dashboard",
		"Title":     "",
		"Cards":     cards,
	}))
}

func (d *Dashboard) outcomesHeadline(ctx context.Context) *landingCard {
	outcomes, err := d.reads.GetOutcomeSummaries(ctx, 0)
	if err != nil || len(outcomes) == 0 {
		return nil
	}
	top := outcomes[0] // sorted by absolute drift
	drift := formatDrift(top.DriftPercent)
	return &landingCard{
		Label:     "Outcomes",
		Headline:  top.Name,
		Sparkline: SparklineSVG(top.HourlyCounts, 200, 40),
		Detail:    fmt.Sprintf("%d events (7d) — %s vs baseline", top.DailyCount, drift),
		Link:      "/outcomes",
	}
}

func (d *Dashboard) perfHeadline(ctx context.Context) *landingCard {
	resp, err := d.reads.GetPerfEndpoints(ctx, reads.GetPerfRequest{})
	if err != nil || len(resp.Endpoints) == 0 {
		return nil
	}
	top := resp.Endpoints[0] // sorted by absolute drift
	return &landingCard{
		Label:     "Performance",
		Headline:  top.Name,
		Sparkline: template.HTML(""), // no hourly series in PerfEndpoint — sparkline comes in card 7
		Detail:    fmt.Sprintf("P95 %.0fms (baseline %.0fms) — %.1fσ drift", top.CurrentP95, top.BaselineP95, top.DriftSigmas),
		Link:      "/performance",
	}
}

func (d *Dashboard) errorsHeadline(ctx context.Context) *landingCard {
	// Try new errors first.
	resp, err := d.reads.GetErrors(ctx, reads.GetErrorsRequest{NewOnly: true})
	if err != nil {
		return nil
	}
	if len(resp.Errors) == 0 {
		// Fall back to all errors.
		resp, err = d.reads.GetErrors(ctx, reads.GetErrorsRequest{})
		if err != nil || len(resp.Errors) == 0 {
			return nil
		}
	}
	top := resp.Errors[0]
	label := top.Name
	if resp.Errors[0].FirstSeen == resp.Errors[0].LastSeen {
		label += " (NEW)"
	}
	return &landingCard{
		Label:     "Errors",
		Headline:  label,
		Sparkline: template.HTML(""),
		Detail:    fmt.Sprintf("%d occurrences — last seen %s", top.Occurrences, top.LastSeen),
		Link:      "/errors",
	}
}

func (d *Dashboard) anomaliesHeadline(ctx context.Context) *landingCard {
	resp, err := d.reads.GetAnomalies(ctx, reads.GetAnomaliesRequest{Since: 24 * time.Hour})
	if err != nil || resp == nil || len(resp.Anomalies) == 0 {
		return &landingCard{
			Label:    "Anomalies",
			Headline: "Nothing unusual",
			Detail:   "All signals are within normal range",
			Link:     "/anomalies",
		}
	}
	top := resp.Anomalies[0] // sorted by deviation descending
	return &landingCard{
		Label:    "Anomalies",
		Headline: top.Name,
		Detail:   fmt.Sprintf("%.1fσ deviation — %d in 24h vs baseline of %.0f", top.DeviationSigma, top.Current, top.BaselineMean),
		Link:     "/anomalies",
	}
}

func formatDrift(pct float64) string {
	abs := math.Abs(pct)
	switch {
	case abs < 1:
		return "flat"
	case pct > 0:
		return fmt.Sprintf("↑ %.0f%%", pct)
	default:
		return fmt.Sprintf("↓ %.0f%%", abs)
	}
}
