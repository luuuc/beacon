package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

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

	detail, err := d.reads.GetErrorDetail(ctx, fingerprint)
	if err != nil {
		d.log.Error("error detail query", "fingerprint", fingerprint, "err", err)
		d.render(w, r, "error_detail.html", "", pageData(map[string]any{
			"ActiveNav":   "errors",
			"Title":       fingerprint,
			"Name":        fingerprint,
			"Fingerprint": fingerprint,
		}))
		return
	}

	// Build chart from hourly occurrences.
	var chart template.HTML
	if len(detail.HourlyOccurrences) > 0 {
		points := make([]ChartPoint, len(detail.HourlyOccurrences))
		for i, pt := range detail.HourlyOccurrences {
			points[i] = ChartPoint{Label: pt.PeriodStart, Value: float64(pt.Count)}
		}
		chart = ChartSVG(ChartOptions{
			Width: 800, Height: 200,
			Series: points,
		})
	}

	// Stats grid.
	stats := []stat{
		{"Exception", detail.Name},
		{"Fingerprint", detail.Fingerprint},
		{"First seen", detail.FirstSeen},
		{"Last seen", detail.LastSeen},
		{"Occurrences", fmt.Sprintf("%d", detail.Occurrences)},
		{"Trend", computeTrend(detail.HourlyOccurrences)},
	}

	// Sample event data (may be nil if pruned).
	var (
		message       string
		firstAppFrame string
		reqMethod     string
		reqPath       string
		deploySHA     string
		environment   string
		requestID     string
		hasSample     bool
		highlightedTrace template.HTML
	)
	if detail.SampleEvent != nil {
		hasSample = true
		message = detail.SampleEvent.Message
		firstAppFrame = detail.SampleEvent.FirstAppFrame

		if detail.SampleEvent.StackTrace != "" {
			highlightedTrace = highlightStackTrace(detail.SampleEvent.StackTrace)
		}
		if detail.SampleEvent.Properties != nil {
			reqPath, _ = detail.SampleEvent.Properties["path"].(string)
		}
		if detail.SampleEvent.Context != nil {
			deploySHA, _ = detail.SampleEvent.Context["deploy_sha"].(string)
			environment, _ = detail.SampleEvent.Context["environment"].(string)
			requestID, _ = detail.SampleEvent.Context["request_id"].(string)
		}
		// Extract method from path if present (e.g. "GET /items/47" → "GET", "/items/47").
		if reqPath != "" {
			if parts := splitMethodPath(reqPath); len(parts) == 2 {
				reqMethod = parts[0]
				reqPath = parts[1]
			}
		}
	}

	d.render(w, r, "error_detail.html", "", pageData(map[string]any{
		"ActiveNav":        "errors",
		"Title":            detail.Name,
		"Name":             detail.Name,
		"Fingerprint":      fingerprint,
		"Chart":            chart,
		"Stats":            stats,
		"HasSample":        hasSample,
		"Message":          message,
		"FirstAppFrame":    firstAppFrame,
		"HighlightedTrace": highlightedTrace,
		"ReqMethod":        reqMethod,
		"ReqPath":          reqPath,
		"DeploySHA":        deploySHA,
		"Environment":      environment,
		"RequestID":        requestID,
	}))
}

// computeTrend compares the second half of the hourly occurrences to the
// first half and returns a human-readable label.
func computeTrend(points []reads.MetricPoint) string {
	if len(points) < 2 {
		return "insufficient data"
	}
	mid := len(points) / 2
	var firstHalf, secondHalf int64
	for _, pt := range points[:mid] {
		firstHalf += pt.Count
	}
	for _, pt := range points[mid:] {
		secondHalf += pt.Count
	}
	if firstHalf == 0 && secondHalf == 0 {
		return "no occurrences"
	}
	if firstHalf == 0 {
		return "increasing (new)"
	}
	ratio := float64(secondHalf) / float64(firstHalf)
	switch {
	case ratio > 1.25:
		return "increasing"
	case ratio < 0.75:
		return "decreasing"
	default:
		return "stable"
	}
}

// splitMethodPath splits "GET /foo" into ["GET", "/foo"]. Returns nil if
// the string doesn't contain a space-separated HTTP method prefix.
func splitMethodPath(s string) []string {
	for i, c := range s {
		if c == ' ' && i > 0 {
			return []string{s[:i], s[i+1:]}
		}
	}
	return nil
}

// highlightStackTrace classifies each line of a stack trace as an app frame
// or a framework frame and wraps them in styled spans. Framework frames
// contain /vendor/, /gems/, /ruby/, or /lib/ruby/.
func highlightStackTrace(trace string) template.HTML {
	lines := strings.Split(trace, "\n")
	var buf strings.Builder
	for _, line := range lines {
		if isFrameworkFrame(line) {
			buf.WriteString(`<span class="frame-framework">`)
		} else {
			buf.WriteString(`<span class="frame-app">`)
		}
		buf.WriteString(template.HTMLEscapeString(line))
		buf.WriteString("</span>\n")
	}
	return template.HTML(buf.String())
}

func isFrameworkFrame(line string) bool {
	return strings.Contains(line, "/vendor/") ||
		strings.Contains(line, "/gems/") ||
		strings.Contains(line, "/ruby/") ||
		strings.Contains(line, "/lib/ruby/")
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
