package dashboard

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type perfRowData struct {
	Name        string
	Kind        string // "http" or "job"
	Method      string // "GET", "POST", etc. or ""
	Path        string // "/checkout" or class name
	MethodClass string // CSS class: "method-GET", "method-job", etc.
	Sparkline   template.HTML
	CurrentP95  string
	BaselineP95 string
	Sigma       float64
	SigmaPill   template.HTML
	Volume      string
}

type perfSummaryStats struct {
	Total  int
	Slower int
	Stable int
	Faster int
}

func (d *Dashboard) handlePerformance(w http.ResponseWriter, r *http.Request) {
	resp, err := d.reads.GetPerfEndpoints(r.Context(), reads.GetPerfRequest{})
	if err != nil {
		d.log.Error("performance query", "err", err)
	}

	filter := r.URL.Query().Get("filter")
	p := message.NewPrinter(language.English)
	var (
		rows  []perfRowData
		stats perfSummaryStats
	)
	if resp != nil {
		stats.Total = len(resp.Endpoints)
		for _, ep := range resp.Endpoints {
			category := "stable"
			switch {
			case ep.DriftSigmas > 0.5:
				stats.Slower++
				category = "slower"
			case ep.DriftSigmas < -0.5:
				stats.Faster++
				category = "faster"
			default:
				stats.Stable++
			}

			kind, method, path := parsePerfName(ep.Name)

			if filter != "" && filter != "all" {
				if filter == "http" && kind != "http" {
					continue
				}
				if filter == "jobs" && kind != "job" {
					continue
				}
				if filter != "http" && filter != "jobs" && filter != category {
					continue
				}
			}

			var spark template.HTML
			if len(ep.HourlyP95) > 0 {
				sparkOpts := SparklineOptions{}
				absSigma := math.Abs(ep.DriftSigmas)
				if absSigma >= 0.5 {
					if ep.DriftSigmas > 0 {
						sparkOpts.Stroke = "var(--danger)"
						sparkOpts.Fill = "var(--danger-soft)"
					} else {
						sparkOpts.Stroke = "var(--ok)"
						sparkOpts.Fill = "var(--ok-soft)"
					}
				} else {
					sparkOpts.Stroke = "var(--text-3)"
					sparkOpts.Fill = "var(--bg-sunken)"
				}
				spark = SparklineSVGStyled(ep.HourlyP95, 92, 26, sparkOpts)
			}

			rows = append(rows, perfRowData{
				Name:        ep.Name,
				Kind:        kind,
				Method:      method,
				Path:        path,
				MethodClass: perfMethodClass(kind, method),
				Sparkline:   spark,
				CurrentP95:  fmtMs(ep.CurrentP95),
				BaselineP95: fmtMs(ep.BaselineP95),
				Sigma:       ep.DriftSigmas,
				SigmaPill:   perfSigmaPill(ep.DriftSigmas),
				Volume:      p.Sprintf("%d", ep.RequestCount),
			})
		}
	}

	data := pageData(map[string]any{
		"ActiveNav": "performance",
		"Title":     "Performance",
		"Rows":      rows,
		"Stats":     stats,
		"Filter":    filter,
	})

	if r.URL.Query().Get("list") == "1" {
		d.render(w, r, "performance.html", "performance-list", data)
		return
	}
	d.render(w, r, "performance.html", "performance-cards", data)
}

func parsePerfName(name string) (kind, method, path string) {
	parts := strings.SplitN(name, " ", 2)
	if len(parts) == 2 {
		method = parts[0]
		path = parts[1]
		return "http", method, path
	}
	return "job", "", name
}

func perfMethodClass(kind, method string) string {
	if kind == "job" {
		return "method-job"
	}
	return "method-" + method
}

func perfSigmaPill(sigma float64) template.HTML {
	abs := math.Abs(sigma)
	if abs < 0.5 {
		return `<span class="perf-pill-stable">stable</span>`
	}
	if sigma > 0 {
		return template.HTML(fmt.Sprintf(`<span class="perf-pill-slower">↑ %.1fσ slower</span>`, abs))
	}
	return template.HTML(fmt.Sprintf(`<span class="perf-pill-faster">↓ %.1fσ faster</span>`, abs))
}

func fmtMs(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.2fs", ms/1000)
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

	var latencyPoints, volumePoints []ChartPoint
	var p95Values []float64
	var totalRequests int64
	for _, pt := range resp.Data {
		val := 0.0
		if pt.P95 != nil {
			val = *pt.P95
		}
		latencyPoints = append(latencyPoints, ChartPoint{Label: pt.PeriodStart, Value: val})
		volumePoints = append(volumePoints, ChartPoint{Label: pt.PeriodStart, Value: float64(pt.Count)})
		p95Values = append(p95Values, val)
		totalRequests += pt.Count
	}

	var baseline, baselineStddev *float64
	var bMean, bStd float64
	if resp.Baseline != nil && resp.Baseline.HourlyCountMean > 0 {
		baseline = &resp.Baseline.HourlyCountMean
		baselineStddev = &resp.Baseline.HourlyCountStd
		bMean = resp.Baseline.HourlyCountMean
		bStd = resp.Baseline.HourlyCountStd
	}

	latencyChart := ChartSVG(ChartOptions{
		Width: 800, Height: 250,
		Series:         latencyPoints,
		Baseline:       baseline,
		BaselineStddev: baselineStddev,
	})
	volumeChart := ChartSVG(ChartOptions{
		Width: 800, Height: 200,
		Series: volumePoints,
	})

	kind, method, path := parsePerfName(name)
	currentP95 := 0.0
	if len(p95Values) > 0 {
		currentP95 = mean(p95Values)
	}
	var sigma float64
	if bStd > 0 {
		sigma = (currentP95 - bMean) / bStd
	}

	p := message.NewPrinter(language.English)

	d.render(w, r, "endpoint_detail.html", "", pageData(map[string]any{
		"ActiveNav":    "performance",
		"Title":        name,
		"Name":         name,
		"Kind":         kind,
		"Method":       method,
		"MethodClass":  perfMethodClass(kind, method),
		"Path":         path,
		"Chart":        latencyChart,
		"VolumeChart":  volumeChart,
		"SigmaPill":    perfSigmaPill(sigma),
		"CurrentP95":   fmtMs(currentP95),
		"BaselineMean": fmt.Sprintf("%.0f", bMean),
		"BaselineStd":  fmt.Sprintf("%.0f", bStd),
		"Requests":     p.Sprintf("%d", totalRequests),
		"Window":       "7d",
		"Period":       resp.PeriodKind,
		"SubtitleText": fmt.Sprintf("P95 %s vs baseline %sms", fmtMs(currentP95), fmt.Sprintf("%.0f", bMean)),
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
