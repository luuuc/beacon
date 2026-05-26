package dashboard

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/luuuc/beacon/internal/reads"
)

type deploySignal struct {
	Pillar string // "outcomes", "performance", "errors"
	Name   string
	Why    string
	Delta  string
	Tone   string // "high", "med", "ok"
}

type verdictData struct {
	Label string
	Tone  string
	Note  string
}

type deployBlockData struct {
	SHA           string
	When          string
	WhenFull      string
	Regressions   []deploySignal
	Improvements  []deploySignal
	Unchanged     int
	NewErrCount   int
	Verdict       verdictData
}

type pillarStatusRow struct {
	Name   string
	Tone   string
	Status string
	Detail string
	Link   string
}

func (d *Dashboard) handleLanding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := d.reads.Now().UTC()

	deploys, _ := d.reads.GetDeployEvents(ctx, now.Add(-30*24*time.Hour), now)
	outcomes, _ := d.reads.GetOutcomeSummaries(ctx, 0)
	perfResp, _ := d.reads.GetPerfEndpoints(ctx, reads.GetPerfRequest{})
	newErrResp, _ := d.reads.GetErrors(ctx, reads.GetErrorsRequest{NewOnly: true})
	allErrResp, _ := d.reads.GetErrors(ctx, reads.GetErrorsRequest{})
	anomResp, _ := d.reads.GetAnomalies(ctx, reads.GetAnomaliesRequest{Since: 24 * time.Hour})

	var perfs []reads.PerfEndpoint
	if perfResp != nil {
		perfs = perfResp.Endpoints
	}
	var newErrors []reads.ErrorSummary
	if newErrResp != nil {
		newErrors = newErrResp.Errors
	}
	var allErrors []reads.ErrorSummary
	if allErrResp != nil {
		allErrors = allErrResp.Errors
	}
	var anomalies []reads.AnomalyEntry
	if anomResp != nil {
		anomalies = anomResp.Anomalies
	}

	totalDims := len(outcomes) + len(perfs) + len(allErrors)

	var regressions, improvements []deploySignal

	for _, ep := range perfs {
		if ep.DriftSigmas > 2.0 {
			regressions = append(regressions, deploySignal{
				Pillar: "performance",
				Name:   ep.Name,
				Why:    fmt.Sprintf("P95 %.0fms", ep.CurrentP95),
				Delta:  fmt.Sprintf("+%.1fσ", ep.DriftSigmas),
				Tone:   sigmaTone(ep.DriftSigmas),
			})
		} else if ep.DriftSigmas < -2.0 {
			improvements = append(improvements, deploySignal{
				Pillar: "performance",
				Name:   ep.Name,
				Why:    "back to baseline",
				Delta:  fmt.Sprintf("%.0f → %.0fms", ep.BaselineP95, ep.CurrentP95),
				Tone:   "ok",
			})
		}
	}

	for _, o := range outcomes {
		if o.Sigma < -2.0 {
			regressions = append(regressions, deploySignal{
				Pillar: "outcomes",
				Name:   o.Name,
				Why:    fmt.Sprintf("%d/day vs ~%.1f/day", o.DailyCount, o.BaselineMean),
				Delta:  fmt.Sprintf("%.0f%%", o.DriftPercent),
				Tone:   sigmaTone(math.Abs(o.Sigma)),
			})
		}
	}

	for _, e := range newErrors {
		regressions = append(regressions, deploySignal{
			Pillar: "errors",
			Name:   e.Name,
			Why:    "new fingerprint",
			Delta:  fmt.Sprintf("%d occ", e.Occurrences),
			Tone:   "high",
		})
	}

	unchanged := totalDims - len(regressions) - len(improvements)
	if unchanged < 0 {
		unchanged = 0
	}

	// Reverse deploys so most recent is first (they arrive ascending).
	for i, j := 0, len(deploys)-1; i < j; i, j = i+1, j-1 {
		deploys[i], deploys[j] = deploys[j], deploys[i]
	}

	var latest *deployBlockData
	var history []deployBlockData

	if len(deploys) > 0 {
		errCount := 0
		for _, r := range regressions {
			if r.Pillar == "errors" {
				errCount++
			}
		}
		latest = &deployBlockData{
			SHA:          shortSHA(deploys[0].SHA),
			When:         relativeTime(deploys[0].CreatedAt, now),
			WhenFull:     deploys[0].CreatedAt.Format("2006-01-02 15:04 UTC"),
			Regressions:  regressions,
			Improvements: improvements,
			Unchanged:    unchanged,
			NewErrCount:  errCount,
		}
		latest.Verdict = computeVerdict(latest.Regressions)

		limit := 4
		if len(deploys) < limit {
			limit = len(deploys)
		}
		for _, dep := range deploys[1:limit] {
			var histReg []deploySignal
			for _, e := range allErrors {
				if e.IntroducedDeploySHA == dep.SHA {
					histReg = append(histReg, deploySignal{
						Pillar: "errors",
						Name:   e.Name,
						Why:    "introduced in this deploy",
						Delta:  fmt.Sprintf("%d occ", e.Occurrences),
						Tone:   "med",
					})
				}
			}
			block := deployBlockData{
				SHA:         shortSHA(dep.SHA),
				When:        relativeTime(dep.CreatedAt, now),
				WhenFull:    dep.CreatedAt.Format("2006-01-02 15:04 UTC"),
				Regressions: histReg,
				Unchanged:   totalDims - len(histReg),
			}
			block.Verdict = computeVerdict(block.Regressions)
			history = append(history, block)
		}
	}

	pillars := buildPillarStatus(outcomes, perfs, newErrors, anomalies)

	d.render(w, r, "landing.html", "", pageData(map[string]any{
		"ActiveNav":  "dashboard",
		"Title":      "",
		"HasDeploys": len(deploys) > 0,
		"Latest":     latest,
		"History":    history,
		"Pillars":    pillars,
		"TotalDims":  totalDims,
	}))
}

func buildPillarStatus(outcomes []reads.OutcomeSummary, perfs []reads.PerfEndpoint, newErrors []reads.ErrorSummary, anomalies []reads.AnomalyEntry) []pillarStatusRow {
	oTone, oStatus, oDetail := "ok", "nominal", fmt.Sprintf("all %d outcomes within range", len(outcomes))
	drifted := 0
	var worstOutcome string
	for _, o := range outcomes {
		if math.Abs(o.Sigma) > 2 {
			drifted++
			if worstOutcome == "" {
				worstOutcome = o.Name
			}
		}
	}
	if drifted > 0 {
		oTone = "med"
		oStatus = fmt.Sprintf("%d outcome%s drifting", drifted, plural(drifted))
		oDetail = worstOutcome
	}
	if len(outcomes) == 0 {
		oDetail = "no outcomes tracked yet"
	}

	pTone, pStatus, pDetail := "ok", "nominal", "P95 within baseline"
	regressed := 0
	var worstPerf string
	for _, ep := range perfs {
		if ep.DriftSigmas > 2 {
			regressed++
			if worstPerf == "" {
				worstPerf = ep.Name
			}
		}
	}
	if regressed > 0 {
		pTone = "high"
		pStatus = fmt.Sprintf("%d P95 regression%s", regressed, plural(regressed))
		pDetail = worstPerf
	}
	if len(perfs) == 0 {
		pDetail = "no endpoints tracked yet"
	}

	eTone, eStatus, eDetail := "ok", "nominal", "0 new fingerprints in 24h"
	if len(newErrors) > 0 {
		eTone = "high"
		eStatus = fmt.Sprintf("%d new fingerprint%s", len(newErrors), plural(len(newErrors)))
		eDetail = newErrors[0].Name
	}

	aTone, aStatus, aDetail := "ok", "0 open", ""
	if len(anomalies) > 0 {
		high := 0
		for _, a := range anomalies {
			if math.Abs(a.DeviationSigma) > 5 {
				high++
			}
		}
		aTone = "med"
		aStatus = fmt.Sprintf("%d open", len(anomalies))
		if high > 0 {
			aTone = "high"
			aStatus += fmt.Sprintf(" · %d high", high)
		}
		aDetail = fmt.Sprintf("worst %.1fσ on %s", anomalies[0].DeviationSigma, anomalies[0].Name)
	}

	return []pillarStatusRow{
		{Name: "Outcomes", Tone: oTone, Status: oStatus, Detail: oDetail, Link: "/outcomes"},
		{Name: "Performance", Tone: pTone, Status: pStatus, Detail: pDetail, Link: "/performance"},
		{Name: "Errors", Tone: eTone, Status: eStatus, Detail: eDetail, Link: "/errors"},
		{Name: "Anomalies", Tone: aTone, Status: aStatus, Detail: aDetail, Link: "/anomalies"},
	}
}

func computeVerdict(regressions []deploySignal) verdictData {
	high := 0
	for _, r := range regressions {
		if r.Tone == "high" {
			high++
		}
	}
	if high > 0 {
		return verdictData{
			Label: "Watch",
			Tone:  "high",
			Note:  fmt.Sprintf("%d high-severity regression%s", high, plural(high)),
		}
	}
	if len(regressions) > 0 {
		return verdictData{
			Label: "Drifting",
			Tone:  "med",
			Note:  fmt.Sprintf("%d signal%s above baseline", len(regressions), plural(len(regressions))),
		}
	}
	return verdictData{
		Label: "Healthy",
		Tone:  "ok",
		Note:  "no regressions detected",
	}
}

func sigmaTone(sigma float64) string {
	if sigma > 5 || sigma < -5 {
		return "high"
	}
	return "med"
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
}

func relativeTime(t time.Time, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
