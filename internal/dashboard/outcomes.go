package dashboard

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
	"github.com/luuuc/beacon/internal/rollup"
)

type outcomeCardData struct {
	Name       string
	Sparkline  template.HTML
	Count      string // e.g. "142/day"
	Drift      string
	DriftClass string
}

func (d *Dashboard) handleOutcomes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const window = 7 * 24 * time.Hour

	summaries, err := d.reads.GetOutcomeSummaries(ctx, window)
	if err != nil {
		d.log.Error("outcomes query", "err", err)
	}

	// Fetch deploy events for sparkline markers.
	now := d.reads.Now().UTC()
	deploys, err := d.reads.GetDeployEvents(ctx, now.Add(-window), now)
	if err != nil {
		d.log.Error("deploy events query", "err", err)
	}

	days := int(window / (24 * time.Hour))
	var cards []outcomeCardData
	for _, s := range summaries {
		drift, cls := driftLabel(s.DriftPercent)

		// Map deploy timestamps to sparkline indices.
		nPts := len(s.HourlyCounts)
		deployIdx := sparklineDeployIndices(deploys, now.Add(-window), now, nPts)

		dailyAvg := s.DailyCount / int64(max(1, days))
		cards = append(cards, outcomeCardData{
			Name:       s.Name,
			Sparkline:  SparklineSVG(s.HourlyCounts, 200, 32, deployIdx...),
			Count:      fmt.Sprintf("%d/day", dailyAvg),
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
	var baselineStddev *float64
	if resp.Baseline != nil && resp.Baseline.HourlyCountMean > 0 {
		daily := resp.Baseline.HourlyCountMean * 24
		baseline = &daily
		dailyStd := resp.Baseline.HourlyCountStd * 24
		baselineStddev = &dailyStd
	}

	// Deploy markers on the chart.
	now := d.reads.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)
	deploys, err := d.reads.GetDeployEvents(ctx, since, now)
	if err != nil {
		d.log.Error("deploy events query", "name", name, "err", err)
	}

	chartDeploys := chartDeployIndices(deploys, points)

	var deployIndices []int
	var deployLabels []string
	for _, cd := range chartDeploys {
		deployIndices = append(deployIndices, cd.Index)
		deployLabels = append(deployLabels, cd.Label)
	}

	chart := ChartSVG(ChartOptions{
		Width: 800, Height: 250,
		Series:         points,
		Baseline:       baseline,
		BaselineStddev: baselineStddev,
		DeployIndices:  deployIndices,
		DeployLabels:   deployLabels,
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

	// Last deploy context block.
	var lastDeploy *deployContext
	if len(deploys) > 0 {
		latest := deploys[len(deploys)-1]
		elapsed := now.Sub(latest.CreatedAt)
		verdict := rollup.VerdictInsufficient
		if resp.Baseline != nil && resp.Baseline.HourlyCountMean > 0 {
			currentMean := float64(totalCount) / float64(max(1, len(resp.Data)))
			dailyBaseline := resp.Baseline.HourlyCountMean * 24
			verdict = rollup.CompareCount(int64(currentMean), int64(dailyBaseline))
		}
		lastDeploy = &deployContext{
			SHA:     shortenSHA(latest.SHA),
			Elapsed: formatElapsed(elapsed),
			Verdict: string(verdict),
		}
	}

	d.render(w, r, "metric_detail.html", "", pageData(map[string]any{
		"ActiveNav":  "outcomes",
		"Title":      name,
		"Name":       name,
		"Chart":      chart,
		"Stats":      stats,
		"LastDeploy": lastDeploy,
	}))
}

type stat struct {
	Label string
	Value string
}

type deployContext struct {
	SHA     string
	Elapsed string
	Verdict string
}

// indexedDeploy pairs a chart series index with a short deploy label.
type indexedDeploy struct {
	Index int
	Label string
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

// sparklineDeployIndices maps deploy timestamps to approximate indices in a
// uniformly-spaced time series spanning [since, until] with nPts points.
func sparklineDeployIndices(deploys []reads.DeployEvent, since, until time.Time, nPts int) []int {
	if nPts <= 1 || len(deploys) == 0 {
		return nil
	}
	span := until.Sub(since)
	if span <= 0 {
		return nil
	}
	var indices []int
	for _, dep := range deploys {
		frac := float64(dep.CreatedAt.Sub(since)) / float64(span)
		idx := int(math.Round(frac * float64(nPts-1)))
		if idx >= 0 && idx < nPts {
			indices = append(indices, idx)
		}
	}
	return indices
}

// chartDeployIndices maps deploy events to chart point indices with labels,
// deduplicating so only one marker per index (last SHA wins).
func chartDeployIndices(deploys []reads.DeployEvent, points []ChartPoint) []indexedDeploy {
	if len(points) == 0 || len(deploys) == 0 {
		return nil
	}
	// Pre-parse point labels once.
	ptTimes := make([]time.Time, len(points))
	for i, pt := range points {
		t, err := time.Parse(time.RFC3339, pt.Label)
		if err != nil {
			t, _ = time.Parse("2006-01-02", pt.Label)
		}
		ptTimes[i] = t
	}

	seen := map[int]int{} // index → position in output
	var out []indexedDeploy
	for _, dep := range deploys {
		idx := nearestIndex(dep.CreatedAt, ptTimes)
		if idx < 0 {
			continue
		}
		lbl := shortenSHA(dep.SHA)
		if pos, ok := seen[idx]; ok {
			out[pos].Label = lbl
		} else {
			seen[idx] = len(out)
			out = append(out, indexedDeploy{Index: idx, Label: lbl})
		}
	}
	return out
}

// nearestIndex returns the index of the closest time in pts, or -1 if pts is empty.
func nearestIndex(t time.Time, pts []time.Time) int {
	best := -1
	var bestDist time.Duration
	for i, pt := range pts {
		if pt.IsZero() {
			continue
		}
		dist := t.Sub(pt)
		if dist < 0 {
			dist = -dist
		}
		if best == -1 || dist < bestDist {
			bestDist = dist
			best = i
		}
	}
	return best
}

// shortenSHA returns a 7-char prefix of a SHA, or "—" if empty.
func shortenSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "—"
	}
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func formatElapsed(d time.Duration) string {
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
