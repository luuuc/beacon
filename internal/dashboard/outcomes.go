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
)

type outcomeRowData struct {
	Name         string
	Description  string
	Sparkline    template.HTML
	PerDay       int64
	BaselineMean string // formatted "X.X/day"
	Sigma        float64
	SigmaPct     string // formatted "+X%" or "-X%"
	SigmaPill    template.HTML
}

type outcomeSummaryStats struct {
	Total    int
	Events24 int64
	Above    int
	Below    int
	Flat     int
}

func (d *Dashboard) handleOutcomes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const window = 7 * 24 * time.Hour

	summaries, err := d.reads.GetOutcomeSummaries(ctx, window)
	if err != nil {
		d.log.Error("outcomes query", "err", err)
	}

	days := int(window / (24 * time.Hour))

	var (
		rows  []outcomeRowData
		stats outcomeSummaryStats
	)
	filter := r.URL.Query().Get("filter")

	stats.Total = len(summaries)
	for _, s := range summaries {
		dailyAvg := s.DailyCount / int64(max(1, days))
		stats.Events24 += dailyAvg

		category := "stable"
		switch {
		case s.Sigma > 0.5:
			stats.Above++
			category = "above"
		case s.Sigma < -0.5:
			stats.Below++
			category = "below"
		default:
			stats.Flat++
		}

		if filter != "" && filter != "all" && filter != category {
			continue
		}

		sparkOpts := SparklineOptions{}
		absSigma := math.Abs(s.Sigma)
		if absSigma >= 0.5 {
			if s.Sigma > 0 {
				sparkOpts.Stroke = "var(--accent)"
				sparkOpts.Fill = "var(--accent-soft)"
			} else {
				sparkOpts.Stroke = "var(--warn)"
				sparkOpts.Fill = "var(--warn-soft)"
			}
		} else {
			sparkOpts.Stroke = "var(--text-3)"
			sparkOpts.Fill = "var(--bg-sunken)"
		}
		spark := SparklineSVGStyled(s.HourlyCounts, 92, 26, sparkOpts)

		pct := s.DriftPercent
		var sigmaPct string
		if pct > 0 {
			sigmaPct = fmt.Sprintf("+%.0f%%", pct)
		} else {
			sigmaPct = fmt.Sprintf("%.0f%%", pct)
		}

		rows = append(rows, outcomeRowData{
			Name:         s.Name,
			Description:  s.Description,
			Sparkline:    spark,
			PerDay:       dailyAvg,
			BaselineMean: fmt.Sprintf("%.1f/day", s.BaselineMean*24),
			Sigma:        s.Sigma,
			SigmaPct:     sigmaPct,
			SigmaPill:    outcomeSigmaPill(s.Sigma, pct),
		})
	}

	data := pageData(map[string]any{
		"ActiveNav": "outcomes",
		"Title":     "Outcomes",
		"Rows":      rows,
		"Stats":     stats,
		"Filter":    filter,
	})

	if r.URL.Query().Get("list") == "1" {
		d.render(w, r, "outcomes.html", "outcomes-list", data)
		return
	}
	d.render(w, r, "outcomes.html", "outcomes-cards", data)
}

func outcomeSigmaPill(sigma, pct float64) template.HTML {
	abs := math.Abs(sigma)
	if abs < 0.5 {
		return `<span class="out-pill-flat">stable</span>`
	}
	if sigma > 0 {
		return template.HTML(fmt.Sprintf(`<span class="out-pill-up">↑ +%.0f%%</span>`, pct))
	}
	return template.HTML(fmt.Sprintf(`<span class="out-pill-down">↓ %.0f%%</span>`, pct))
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
		d.render(w, r, "outcome_detail.html", "", pageData(map[string]any{
			"ActiveNav": "outcomes",
			"Title":     name,
			"Name":      name,
		}))
		return
	}

	points := make([]ChartPoint, len(resp.Data))
	var totalCount int64
	for i, pt := range resp.Data {
		points[i] = ChartPoint{Label: pt.PeriodStart, Value: float64(pt.Count)}
		totalCount += pt.Count
	}

	var baseline, baselineStddev *float64
	var bMean, bStd float64
	var capturedAt string
	if resp.Baseline != nil && resp.Baseline.HourlyCountMean > 0 {
		daily := resp.Baseline.HourlyCountMean * 24
		baseline = &daily
		dailyStd := resp.Baseline.HourlyCountStd * 24
		baselineStddev = &dailyStd
		bMean = resp.Baseline.HourlyCountMean
		bStd = resp.Baseline.HourlyCountStd
		capturedAt = resp.Baseline.CapturedAt
	}

	now := d.reads.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)
	deploys, _ := d.reads.GetDeployEvents(ctx, since, now)
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

	// Compute current per-period and sigma for the header pill.
	var currentPerPeriod float64
	if len(resp.Data) > 0 {
		currentPerPeriod = float64(resp.Data[len(resp.Data)-1].Count)
	}
	var sigma float64
	if bStd > 0 && baseline != nil {
		sigma = (currentPerPeriod - *baseline) / *baselineStddev
	}
	deltaPct := 0.0
	if baseline != nil && *baseline > 0 {
		deltaPct = ((currentPerPeriod - *baseline) / *baseline) * 100
	}

	d.render(w, r, "outcome_detail.html", "", pageData(map[string]any{
		"ActiveNav":      "outcomes",
		"Title":          name,
		"Name":           name,
		"Chart":          chart,
		"TotalWindow":    totalCount,
		"Period":         resp.PeriodKind,
		"Window":         "7d",
		"BaselineMean":   fmt.Sprintf("%.1f", bMean),
		"BaselineStddev": fmt.Sprintf("%.1f", bStd),
		"CapturedAt":     capturedAt,
		"SigmaPill":      outcomeSigmaPill(sigma, deltaPct),
		"CurrentLabel":   fmt.Sprintf("%.0f/%s vs baseline %.1f/%s", currentPerPeriod, resp.PeriodKind, safeDeref(baseline), resp.PeriodKind),
	}))
}

func safeDeref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
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
