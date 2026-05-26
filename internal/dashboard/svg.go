package dashboard

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

// SparklineOptions configures sparkline appearance.
type SparklineOptions struct {
	Stroke string // CSS color for the line; defaults to "var(--accent)"
	Fill   string // CSS color for the area fill; defaults to "var(--accent-soft)"
}

// SparklineSVG returns an inline <svg> element with a filled area + line for
// the given data series and optional deploy marker lines.
func SparklineSVG(series []float64, width, height int, deployIndices ...int) template.HTML {
	return SparklineSVGStyled(series, width, height, SparklineOptions{}, deployIndices...)
}

// SparklineSVGStyled is like SparklineSVG but accepts custom stroke/fill colors.
func SparklineSVGStyled(series []float64, width, height int, opts SparklineOptions, deployIndices ...int) template.HTML {
	if opts.Stroke == "" {
		opts.Stroke = "var(--accent)"
	}
	if opts.Fill == "" {
		opts.Fill = "var(--accent-soft)"
	}

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

	span := maxVal - minVal
	if span == 0 {
		span = 1
	}

	padding := 1.0
	drawW := float64(width) - 2*padding
	drawH := float64(height) - 2*padding

	points := make([]string, len(series))
	for i, v := range series {
		var x float64
		if len(series) == 1 {
			x = drawW / 2
		} else {
			x = drawW * float64(i) / float64(len(series)-1)
		}
		y := drawH - drawH*(v-minVal)/span
		points[i] = fmt.Sprintf("%.1f,%.1f", padding+x, padding+y)
	}

	var b strings.Builder
	n := len(series)
	fmt.Fprintf(&b, `<svg class="sparkline" width="%d" height="%d" viewBox="0 0 %d %d" preserveAspectRatio="none">`,
		width, height, width, height)

	for _, idx := range deployIndices {
		if idx >= 0 && idx < n {
			var dx float64
			if n == 1 {
				dx = drawW / 2
			} else {
				dx = drawW * float64(idx) / float64(n-1)
			}
			fmt.Fprintf(&b,
				`<line x1="%.1f" y1="0" x2="%.1f" y2="%d" class="sparkline-deploy"/>`,
				padding+dx, padding+dx, height,
			)
		}
	}

	// Area fill: close the path down to the baseline.
	firstX := padding
	if len(series) == 1 {
		firstX = padding + drawW/2
	}
	lastX := padding + drawW
	if len(series) == 1 {
		lastX = firstX
	}
	fmt.Fprintf(&b,
		`<polygon fill="%s" opacity="0.7" points="%s %.1f,%.1f %.1f,%.1f"/>`,
		opts.Fill,
		strings.Join(points, " "),
		lastX, float64(height)-padding,
		firstX, float64(height)-padding,
	)

	// Stroke line on top.
	fmt.Fprintf(&b,
		`<polyline fill="none" stroke="%s" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" points="%s"/>`,
		opts.Stroke,
		strings.Join(points, " "))

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
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
		bLow := *opts.Baseline
		bHigh := *opts.Baseline
		if opts.BaselineStddev != nil {
			bLow -= *opts.BaselineStddev
			if bLow < 0 {
				bLow = 0
			}
			bHigh += *opts.BaselineStddev
		}
		if bLow < minVal {
			minVal = bLow
		}
		if bHigh > maxVal {
			maxVal = bHigh
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

	// X axis labels — up to 4 evenly spaced.
	n := len(opts.Series)
	xLabelIdx := []int{0}
	if n > 2 {
		xLabelIdx = append(xLabelIdx, n/3, 2*n/3)
	}
	if n > 1 {
		xLabelIdx = append(xLabelIdx, n-1)
	}
	for i, idx := range xLabelIdx {
		x := padLeft + drawW*float64(idx)/float64(max(1, n-1))
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == len(xLabelIdx)-1 {
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="chart-label-x" text-anchor="%s">%s</text>`, x, h-5, anchor, shortenChartLabel(opts.Series[idx].Label))
	}

	// Baseline band (±1σ) — rendered first so it sits behind the data line.
	if opts.Baseline != nil && opts.BaselineStddev != nil && *opts.BaselineStddev > 0 {
		upper := *opts.Baseline + *opts.BaselineStddev
		lower := *opts.Baseline - *opts.BaselineStddev
		if lower < 0 {
			lower = 0 // clip to zero for low-traffic metrics
		}
		yUpper := padTop + drawH*(1-(upper-minVal)/span)
		yLower := padTop + drawH*(1-(lower-minVal)/span)
		// Clamp to chart area.
		if yUpper < padTop {
			yUpper = padTop
		}
		if yLower > padTop+drawH {
			yLower = padTop + drawH
		}
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.0f" width="%.0f" height="%.0f" class="chart-band"/>`,
			padLeft, yUpper, drawW, yLower-yUpper)
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

	// Deploy markers with optional SHA labels.
	for i, idx := range opts.DeployIndices {
		if idx >= 0 && idx < n {
			x := padLeft + drawW*float64(idx)/float64(max(1, n-1))
			fmt.Fprintf(&b, `<line x1="%.0f" y1="%.0f" x2="%.0f" y2="%.0f" class="chart-deploy"/>`, x, padTop, x, padTop+drawH)
			if i < len(opts.DeployLabels) && opts.DeployLabels[i] != "" {
				fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="chart-deploy-label">%s</text>`, x+3, padTop+12, opts.DeployLabels[i])
			}
		}
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// ChartOptions configures a full time-series chart.
type ChartOptions struct {
	Width, Height  int
	Series         []ChartPoint
	Baseline       *float64
	BaselineStddev *float64 // when set, renders a ±1σ band around Baseline
	DeployIndices  []int    // indices into Series where deploys occurred
	DeployLabels   []string // short labels (SHA prefix) per deploy index
}

// ChartPoint is a single data point in a chart.
type ChartPoint struct {
	Label string
	Value float64
}

// shortenChartLabel trims RFC3339 or date timestamps to a compact form.
// "2026-05-19T12:00:00Z" → "05-19 12:00", "2026-05-19" → "05-19"
func shortenChartLabel(s string) string {
	if len(s) >= 16 && s[4] == '-' && s[10] == 'T' {
		return s[5:10] + " " + s[11:16]
	}
	if len(s) >= 10 && s[4] == '-' {
		return s[5:10]
	}
	return s
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
