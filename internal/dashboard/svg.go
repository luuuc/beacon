package dashboard

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

// SparklineSVG returns an inline <svg> element with a polyline for the given
// data series. The result is safe to embed directly in templates.
func SparklineSVG(series []float64, width, height int) template.HTML {
	if len(series) == 0 {
		return template.HTML(fmt.Sprintf(
			`<svg class="sparkline" width="%d" height="%d" viewBox="0 0 %d %d"></svg>`,
			width, height, width, height,
		))
	}

	minVal, maxVal := series[0], series[0]
	for _, v := range series {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	// Avoid division by zero for flat lines.
	span := maxVal - minVal
	if span == 0 {
		span = 1
	}

	padding := 2.0
	drawW := float64(width) - 2*padding
	drawH := float64(height) - 2*padding

	points := make([]string, len(series))
	for i, v := range series {
		var x, y float64
		if len(series) == 1 {
			x = drawW / 2
		} else {
			x = drawW * float64(i) / float64(len(series)-1)
		}
		y = drawH - drawH*(v-minVal)/span
		points[i] = fmt.Sprintf("%.1f,%.1f", padding+x, padding+y)
	}

	return template.HTML(fmt.Sprintf(
		`<svg class="sparkline" width="%d" height="%d" viewBox="0 0 %d %d" preserveAspectRatio="none">`+
			`<polyline fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" points="%s"/>`+
			`</svg>`,
		width, height, width, height,
		strings.Join(points, " "),
	))
}

// ChartSVG renders a larger time-series chart with axis labels, a baseline
// dashed line, and optional deploy markers. Used on detail pages.
func ChartSVG(opts ChartOptions) template.HTML {
	if len(opts.Series) == 0 {
		return template.HTML(fmt.Sprintf(
			`<svg class="chart" width="%d" height="%d" viewBox="0 0 %d %d"></svg>`,
			opts.Width, opts.Height, opts.Width, opts.Height,
		))
	}

	const (
		padLeft   = 60.0
		padRight  = 20.0
		padTop    = 20.0
		padBottom = 30.0
	)

	w := float64(opts.Width)
	h := float64(opts.Height)
	drawW := w - padLeft - padRight
	drawH := h - padTop - padBottom

	// Find range.
	minVal, maxVal := opts.Series[0].Value, opts.Series[0].Value
	for _, p := range opts.Series {
		if p.Value < minVal {
			minVal = p.Value
		}
		if p.Value > maxVal {
			maxVal = p.Value
		}
	}
	if opts.Baseline != nil {
		if *opts.Baseline < minVal {
			minVal = *opts.Baseline
		}
		if *opts.Baseline > maxVal {
			maxVal = *opts.Baseline
		}
	}
	span := maxVal - minVal
	if span == 0 {
		span = 1
	}
	// Add 10% padding to Y range.
	minVal -= span * 0.05
	maxVal += span * 0.05
	span = maxVal - minVal

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" width="%d" height="%d" viewBox="0 0 %d %d">`, opts.Width, opts.Height, opts.Width, opts.Height)

	// Y axis — 5 ticks.
	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4.0
		val := minVal + span*frac
		y := padTop + drawH*(1-frac)
		label := formatValue(val)
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="chart-label-y" text-anchor="end" dominant-baseline="middle">%s</text>`, padLeft-8, y, label)
		fmt.Fprintf(&b, `<line x1="%.0f" y1="%.0f" x2="%.0f" y2="%.0f" class="chart-grid"/>`, padLeft, y, w-padRight, y)
	}

	// X axis labels — up to 7.
	n := len(opts.Series)
	step := max(1, n/7)
	for i := 0; i < n; i += step {
		x := padLeft + drawW*float64(i)/float64(max(1, n-1))
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="chart-label-x" text-anchor="middle">%s</text>`, x, h-5, opts.Series[i].Label)
	}

	// Data line.
	points := make([]string, n)
	for i, p := range opts.Series {
		x := padLeft + drawW*float64(i)/float64(max(1, n-1))
		y := padTop + drawH*(1-(p.Value-minVal)/span)
		points[i] = fmt.Sprintf("%.1f,%.1f", x, y)
	}
	fmt.Fprintf(&b, `<polyline fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" points="%s" class="chart-line"/>`, strings.Join(points, " "))

	// Baseline.
	if opts.Baseline != nil {
		y := padTop + drawH*(1-(*opts.Baseline-minVal)/span)
		fmt.Fprintf(&b, `<line x1="%.0f" y1="%.0f" x2="%.0f" y2="%.0f" class="chart-baseline"/>`, padLeft, y, w-padRight, y)
	}

	// Deploy markers.
	for _, idx := range opts.DeployIndices {
		if idx >= 0 && idx < n {
			x := padLeft + drawW*float64(idx)/float64(max(1, n-1))
			fmt.Fprintf(&b, `<line x1="%.0f" y1="%.0f" x2="%.0f" y2="%.0f" class="chart-deploy"/>`, x, padTop, x, padTop+drawH)
		}
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// ChartOptions configures a full time-series chart.
type ChartOptions struct {
	Width, Height int
	Series        []ChartPoint
	Baseline      *float64
	DeployIndices []int // indices into Series where deploys occurred
}

// ChartPoint is a single data point in a chart.
type ChartPoint struct {
	Label string
	Value float64
}

func formatValue(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case abs >= 1_000:
		return fmt.Sprintf("%.1fK", v/1_000)
	case abs == math.Trunc(abs):
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}
