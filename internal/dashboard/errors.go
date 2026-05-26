package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/reads"
)

type errorRowData struct {
	Name         string
	Message      string
	Fingerprint  string
	FPShort      string
	DeploySHA    string
	Status       string // "new", "unresolved", "resolved"
	Sparkline    template.HTML
	Count        int64
	TrendText    string
	TrendIcon    string
	TrendClass   string
	LastSeenRel  string
	LastSeenAbs  string
	FirstSeenRel string
}

type errorSummaryStats struct {
	Total      int
	New        int
	Unresolved int
	Resolved   int
	Events     int64
	Increasing int
}

func (d *Dashboard) handleErrors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resp, err := d.reads.GetErrors(ctx, reads.GetErrorsRequest{})
	if err != nil {
		d.log.Error("errors query", "err", err)
	}

	// Find the latest deploy SHA to derive error status.
	now := d.reads.Now().UTC()
	deploys, _ := d.reads.GetDeployEvents(ctx, now.Add(-30*24*time.Hour), now)
	var latestDeploySHA string
	var latestDeployTime time.Time
	if len(deploys) > 0 {
		latest := deploys[len(deploys)-1]
		latestDeploySHA = latest.SHA
		latestDeployTime = latest.CreatedAt
	}

	// Also fetch resolved errors — errors from before the deploy window
	// that haven't fired since the last deploy.
	var resolvedErrors []reads.ErrorSummary
	if !latestDeployTime.IsZero() {
		olderResp, _ := d.reads.GetErrors(ctx, reads.GetErrorsRequest{
			Since: 30 * 24 * time.Hour,
		})
		if olderResp != nil {
			for _, e := range olderResp.Errors {
				lastSeen, _ := time.Parse(time.RFC3339, e.LastSeen)
				if !lastSeen.IsZero() && lastSeen.Before(latestDeployTime) {
					resolvedErrors = append(resolvedErrors, e)
				}
			}
		}
	}

	filter := r.URL.Query().Get("filter")

	var (
		rows  []errorRowData
		stats errorSummaryStats
	)
	if resp != nil {
		for _, e := range resp.Errors {
			status := errorStatus(e, latestDeploySHA, latestDeployTime, now)
			stats.Events += e.Occurrences
			if e.Trend == "increasing" {
				stats.Increasing++
			}
			switch status {
			case "new":
				stats.New++
			case "unresolved":
				stats.Unresolved++
			}

			if filter != "" && filter != "all" && filter != status {
				continue
			}

			rows = append(rows, buildErrorRow(e, status))
		}
		// Add resolved errors to stats and rows.
		for _, e := range resolvedErrors {
			stats.Resolved++
			if filter == "resolved" {
				rows = append(rows, buildErrorRow(e, "resolved"))
			}
		}
		stats.Total = stats.New + stats.Unresolved + stats.Resolved
	}

	data := pageData(map[string]any{
		"ActiveNav": "errors",
		"Title":     "Errors",
		"Filter":    filter,
		"Errors":    rows,
		"Stats":     stats,
	})

	if r.URL.Query().Get("list") == "1" {
		d.render(w, r, "errors.html", "errors-list", data)
		return
	}
	d.render(w, r, "errors.html", "errors-cards", data)
}

// errorStatus derives status from deploy SHA comparison.
//   - new: introduced in the latest deploy
//   - unresolved: older error that re-appeared after latest deploy
//   - resolved: older error not triggered since latest deploy
// errorStatus derives status from deploy SHA comparison. When no deploy
// events exist, falls back to time-based heuristic (new = first seen < 24h ago).
func errorStatus(e reads.ErrorSummary, latestDeploySHA string, latestDeployTime, now time.Time) string {
	if latestDeploySHA == "" {
		firstSeen, _ := time.Parse(time.RFC3339, e.FirstSeen)
		if !firstSeen.IsZero() && now.Sub(firstSeen) < 24*time.Hour {
			return "new"
		}
		return "unresolved"
	}
	if e.IntroducedDeploySHA == latestDeploySHA {
		return "new"
	}
	lastSeen, _ := time.Parse(time.RFC3339, e.LastSeen)
	if !lastSeen.IsZero() && lastSeen.Before(latestDeployTime) {
		return "resolved"
	}
	return "unresolved"
}

func buildErrorRow(e reads.ErrorSummary, status string) errorRowData {
	var spark template.HTML
	if len(e.HourlyCounts) > 0 {
		series := make([]float64, len(e.HourlyCounts))
		for i, c := range e.HourlyCounts {
			series[i] = float64(c)
		}
		spark = SparklineSVGStyled(series, 92, 26, SparklineOptions{
			Stroke: "var(--accent)",
			Fill:   "var(--accent-soft)",
		})
	}

	trendIcon, trendClass := trendDisplay(e.Trend)

	fpShort := e.Fingerprint
	if len(fpShort) > 12 {
		fpShort = fpShort[:12]
	}

	deploySHA := e.IntroducedDeploySHA
	if len(deploySHA) > 8 {
		deploySHA = deploySHA[:8]
	}

	return errorRowData{
		Name:         e.Name,
		Message:      e.Message,
		Fingerprint:  e.Fingerprint,
		FPShort:      fpShort,
		DeploySHA:    deploySHA,
		Status:       status,
		Sparkline:    spark,
		Count:        e.Occurrences,
		TrendText:    e.Trend,
		TrendIcon:    trendIcon,
		TrendClass:   trendClass,
		LastSeenRel:  formatTimeAgo(e.LastSeen),
		LastSeenAbs:  e.LastSeen[5:min(16, len(e.LastSeen))] + "Z",
		FirstSeenRel: formatTimeAgo(e.FirstSeen),
	}
}

// stackFrame holds a parsed line from a stack trace for tabular rendering.
type stackFrame struct {
	Num      string
	File     string
	Line     string
	Function string
	InApp    bool
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

	// Sparkline for the metric rail.
	var sparkline template.HTML
	if len(detail.HourlyOccurrences) > 0 {
		series := make([]float64, len(detail.HourlyOccurrences))
		for i, pt := range detail.HourlyOccurrences {
			series[i] = float64(pt.Count)
		}
		sparkline = SparklineSVGStyled(series, 120, 28, SparklineOptions{
			Stroke: "var(--accent)",
			Fill:   "var(--accent-soft)",
		})
	}

	// Trend for the metric rail.
	trendText := computeTrend(detail.HourlyOccurrences)
	trendIcon, trendClass := trendDisplay(trendText)

	// Intro deploy SHA formatting.
	introSHAShort := "unknown"
	introSHARest := ""
	if detail.IntroducedDeploySHA != "" {
		introSHAShort = detail.IntroducedDeploySHA
		if len(introSHAShort) > 8 {
			introSHARest = introSHAShort[8:]
			if len(introSHARest) > 8 {
				introSHARest = introSHARest[:8]
			}
			introSHAShort = introSHAShort[:8]
		}
	}

	// Formatted dates.
	firstSeenFmt := formatDateShort(detail.FirstSeen)
	lastSeenFmt := formatDateShort(detail.LastSeen)
	firstSeenAgo := formatTimeAgo(detail.FirstSeen)
	lastSeenAgo := formatTimeAgo(detail.LastSeen)

	// Sample event data (may be nil if pruned).
	var (
		message     string
		hasSample   bool
		frames      []stackFrame
		inAppCount  int
		reqMethod   string
		reqPath     string
		deploySHA   string
		environment string
		requestID   string
	)
	if detail.SampleEvent != nil {
		hasSample = true
		message = detail.SampleEvent.Message

		if detail.SampleEvent.StackTrace != "" {
			frames = parseStackTrace(detail.SampleEvent.StackTrace)
			for _, f := range frames {
				if f.InApp {
					inAppCount++
				}
			}
		}
		if detail.SampleEvent.Properties != nil {
			reqPath, _ = detail.SampleEvent.Properties["path"].(string)
		}
		if detail.SampleEvent.Context != nil {
			deploySHA, _ = detail.SampleEvent.Context["deploy_sha"].(string)
			environment, _ = detail.SampleEvent.Context["environment"].(string)
			requestID, _ = detail.SampleEvent.Context["request_id"].(string)
		}
		if reqPath != "" {
			if parts := splitMethodPath(reqPath); len(parts) == 2 {
				reqMethod = parts[0]
				reqPath = parts[1]
			}
		}
	}

	d.render(w, r, "error_detail.html", "", pageData(map[string]any{
		"ActiveNav":     "errors",
		"Title":         detail.Name,
		"Name":          detail.Name,
		"Fingerprint":   fingerprint,
		"Message":       message,
		"Occurrences":   detail.Occurrences,
		"TrendText":     trendText,
		"TrendIcon":     trendIcon,
		"TrendClass":    trendClass,
		"FirstSeen":     firstSeenFmt,
		"FirstSeenAgo":  firstSeenAgo,
		"LastSeen":      lastSeenFmt,
		"LastSeenAgo":   lastSeenAgo,
		"IntroSHAShort": introSHAShort,
		"IntroSHARest":  introSHARest,
		"Sparkline":     sparkline,
		"Chart":         chart,
		"BucketCount":   len(detail.HourlyOccurrences),
		"HasSample":     hasSample,
		"Frames":        frames,
		"InAppFrames":   inAppCount,
		"TotalFrames":   len(frames),
		"ReqMethod":     reqMethod,
		"ReqPath":       reqPath,
		"DeploySHA":     deploySHA,
		"Environment":   environment,
		"RequestID":     requestID,
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

// formatDateShort formats an RFC3339 timestamp as "2006-01-02 15:04Z".
func formatDateShort(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// trendDisplay returns a directional icon and CSS class for a trend label.
func trendDisplay(trend string) (icon, class string) {
	switch {
	case strings.Contains(trend, "increasing"):
		return "↑", "ed-trend-up"
	case strings.Contains(trend, "decreasing"):
		return "↓", "ed-trend-down"
	default:
		return "→", "ed-trend-flat"
	}
}

// parseStackTrace splits a raw stack trace string into structured frames.
// Handles Ruby-style "file:line:in `method'" and generic "file:line" formats.
func parseStackTrace(trace string) []stackFrame {
	lines := strings.Split(trace, "\n")
	var frames []stackFrame
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := stackFrame{
			Num:   fmt.Sprintf("%02d", i),
			InApp: !isFrameworkFrame(line),
		}
		// Try Ruby format: file:line:in `method'
		if idx := strings.Index(line, ":in `"); idx > 0 {
			fileLine := line[:idx]
			fn := line[idx+5:]
			fn = strings.TrimSuffix(fn, "'")
			if colonIdx := strings.LastIndex(fileLine, ":"); colonIdx > 0 {
				f.File = fileLine[:colonIdx]
				f.Line = fileLine[colonIdx+1:]
			} else {
				f.File = fileLine
			}
			f.Function = fn
		} else if idx := strings.LastIndex(line, ":"); idx > 0 {
			// Generic file:line format.
			f.File = line[:idx]
			f.Line = line[idx+1:]
			f.Function = line
		} else {
			f.File = line
			f.Function = line
		}
		frames = append(frames, f)
	}
	return frames
}
