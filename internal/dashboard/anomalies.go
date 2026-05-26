package dashboard

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/reads"
)

type anomalyRowData struct {
	ID            int64
	AnomalyKind   string // raw kind: volume_shift, dimension_spike, etc.
	KindLabel     string // display label: VOLUME SHIFT, DIMENSION SPIKE, etc.
	Pillar        string // outcomes, performance, errors
	SevTier       string // high, medium, low
	Name          string
	Dimension     string
	Current       int64
	CurrentFmt    string // formatted: "1.3k" or "316"
	CurrentP95    string // perf_drift only
	BaselineMean  string // "~184/day" or "~198ms"
	Sigma         string // "37.7σ"
	SigmaRaw      float64
	Multiplier    string // "19×"
	MultiplierRaw int64
	Summary       string
	Link          string
}

type anomalyStats struct {
	Total       int
	High        int
	Medium      int
	Low         int
	WorstSigma  string
	WorstName   string
	TopMult     int64
	Outcomes    int
	Performance int
	Errors      int
}

func (d *Dashboard) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp, err := d.reads.GetAnomalies(ctx, reads.GetAnomaliesRequest{Since: 7 * 24 * time.Hour})
	if err != nil {
		d.log.Error("anomalies query", "err", err)
	}

	var rows []anomalyRowData
	if resp != nil {
		for _, a := range resp.Anomalies {
			rows = append(rows, toAnomalyRow(a))
		}
	}

	stats := computeAnomalyStats(rows)

	filter := r.URL.Query().Get("filter")
	filtered := filterAnomalies(rows, filter)

	data := pageData(map[string]any{
		"ActiveNav": "anomalies",
		"Title":     "Anomalies",
		"Anomalies": filtered,
		"Stats":     stats,
		"Filter":    filter,
	})

	if r.URL.Query().Get("list") == "1" {
		d.render(w, r, "anomalies.html", "anomalies-list", data)
		return
	}
	d.render(w, r, "anomalies.html", "anomalies-cards", data)
}

func toAnomalyRow(a reads.AnomalyEntry) anomalyRowData {
	pillar := pillarForKind(a.MetricKind)
	tier := sevTier(a.DeviationSigma)

	var mult int64
	if a.BaselineMean > 0 {
		mult = int64(math.Round(float64(a.Current) / a.BaselineMean))
		if mult < 1 {
			mult = 1
		}
	}

	var baseStr string
	if a.AnomalyKind == "perf_drift" {
		baseStr = fmt.Sprintf("~%.0fms", a.BaselineMean)
	} else {
		baseStr = fmt.Sprintf("~%.0f/day", a.BaselineMean)
	}

	var dimStr string
	if len(a.Dimension) > 0 {
		keys := make([]string, 0, len(a.Dimension))
		for k := range a.Dimension {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, a.Dimension[k]))
		}
		dimStr = strings.Join(parts, ", ")
	}

	return anomalyRowData{
		ID:            a.ID,
		AnomalyKind:   a.AnomalyKind,
		KindLabel:     kindLabel(a.AnomalyKind),
		Pillar:        pillar,
		SevTier:       tier,
		Name:          a.Name,
		Dimension:     dimStr,
		Current:       a.Current,
		CurrentFmt:    fmtCount(a.Current),
		CurrentP95:    fmt.Sprintf("%.0f", a.CurrentP95),
		BaselineMean:  baseStr,
		Sigma:         fmt.Sprintf("%.1fσ", a.DeviationSigma),
		SigmaRaw:      a.DeviationSigma,
		Multiplier:    fmt.Sprintf("%d×", mult),
		MultiplierRaw: mult,
		Summary:       a.Summary,
		Link:          linkForAnomaly(a),
	}
}

func computeAnomalyStats(rows []anomalyRowData) anomalyStats {
	s := anomalyStats{Total: len(rows)}
	var worstSigma float64
	for _, r := range rows {
		switch r.SevTier {
		case "high":
			s.High++
		case "medium":
			s.Medium++
		default:
			s.Low++
		}
		switch r.Pillar {
		case "outcomes":
			s.Outcomes++
		case "performance":
			s.Performance++
		case "errors":
			s.Errors++
		}
		if r.SigmaRaw > worstSigma {
			worstSigma = r.SigmaRaw
			s.WorstName = r.Name
		}
		if r.MultiplierRaw > s.TopMult {
			s.TopMult = r.MultiplierRaw
		}
	}
	s.WorstSigma = fmt.Sprintf("%.1fσ", worstSigma)
	return s
}

func filterAnomalies(rows []anomalyRowData, filter string) []anomalyRowData {
	if filter == "" || filter == "all" {
		return rows
	}
	var out []anomalyRowData
	for _, r := range rows {
		switch filter {
		case "severe":
			if r.SevTier == "high" {
				out = append(out, r)
			}
		case "strong":
			if r.SevTier == "medium" {
				out = append(out, r)
			}
		case "mild":
			if r.SevTier == "low" {
				out = append(out, r)
			}
		case "outcomes":
			if r.Pillar == "outcomes" {
				out = append(out, r)
			}
		case "performance":
			if r.Pillar == "performance" {
				out = append(out, r)
			}
		case "errors":
			if r.Pillar == "errors" {
				out = append(out, r)
			}
		default:
			return rows
		}
	}
	return out
}

func pillarForKind(metricKind string) string {
	switch metricKind {
	case "outcome":
		return "outcomes"
	case "perf":
		return "performance"
	case "error":
		return "errors"
	default:
		return "performance"
	}
}

func sevTier(sigma float64) string {
	if sigma >= 10 {
		return "high"
	}
	if sigma >= 5 {
		return "medium"
	}
	return "low"
}

func kindLabel(kind string) string {
	switch kind {
	case "volume_shift":
		return "VOLUME SHIFT"
	case "dimension_spike":
		return "DIMENSION SPIKE"
	case "perf_drift":
		return "PERF DRIFT"
	case "error_rate_spike":
		return "ERROR SPIKE"
	case "outcome_drop":
		return "OUTCOME DROP"
	default:
		return strings.ToUpper(strings.ReplaceAll(kind, "_", " "))
	}
}

func fmtCount(n int64) string {
	if n >= 10000 {
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.FormatInt(n, 10)
}

func (d *Dashboard) handleDismissAnomaly(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := d.reads.DismissAnomaly(r.Context(), id); err != nil {
		if errors.Is(err, beacondb.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		d.log.Error("dismiss anomaly", "err", err, "id", id)
		http.Error(w, "dismiss failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func linkForAnomaly(a reads.AnomalyEntry) string {
	switch a.MetricKind {
	case "perf":
		return "/performance/" + url.PathEscape(a.Name)
	case "error":
		return "/errors"
	case "outcome":
		return "/outcomes/" + url.PathEscape(a.Name)
	default:
		return ""
	}
}
