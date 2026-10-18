package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

// prChecks is the JSON of the host command pr.checks.
type prChecks struct {
	Repo    string  `json:"repo"`
	Number  int     `json:"number"`
	URL     string  `json:"url"`
	HeadSHA string  `json:"headSha"`
	Checks  []check `json:"checks"`
}

type check struct {
	Name       string `json:"name"`
	Workflow   string `json:"workflow"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        string `json:"app"`
	Provider   string `json:"provider"`
	URL        string `json:"url"`
}

// threadChecks asks Code Foundry for the checks of a thread's pull request.
func threadChecks(ctx context.Context, h *pluginsdk.Host, thread string) (prChecks, error) {
	var pc prChecks
	res, err := h.Invoke(ctx, "pr.checks", map[string]string{"thread": thread}, pluginsdk.UiContext{ThreadID: thread})
	if err != nil {
		return pc, err
	}
	if err := json.Unmarshal(res.JSON, &pc); err != nil {
		return pc, fmt.Errorf("reading pr.checks: %w", err)
	}
	return pc, nil
}

// prRef names a pull request.
type prRef struct {
	Repo   string
	Number int
	URL    string
}

func (p prRef) String() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

// prFromFacts reads the pr scope's fact {"repo","number","url"}.
func prFromFacts(facts map[string]json.RawMessage) (prRef, bool) {
	raw, ok := facts["pr"]
	if !ok {
		return prRef{}, false
	}
	var f struct {
		Repo   string          `json:"repo"`
		Number json.RawMessage `json:"number"`
		URL    string          `json:"url"`
	}
	if json.Unmarshal(raw, &f) != nil || f.Repo == "" {
		return prRef{}, false
	}
	n, err := strconv.Atoi(strings.Trim(string(f.Number), `"`))
	if err != nil || n <= 0 {
		return prRef{}, false
	}
	return prRef{Repo: f.Repo, Number: n, URL: f.URL}, true
}

// Status words.

func isFailure(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

func completed(status string) bool { return status == "completed" }

// statusOf maps a run's or job's status and conclusion to a glyph status.
func statusOf(status, conclusion string) ui.Status {
	if !completed(status) {
		if status == "in_progress" {
			return ui.StatusRunning
		}
		return ui.StatusPending
	}
	switch {
	case conclusion == "success" || conclusion == "neutral":
		return ui.StatusOK
	case isFailure(conclusion):
		return ui.StatusFailed
	case conclusion == "action_required":
		return ui.StatusAttention
	case conclusion == "skipped":
		return ui.StatusSkipped
	default: // cancelled, stale
		return ui.StatusOff
	}
}

// pillOf is a one- or two-word pill for a run's or job's state.
func pillOf(status, conclusion string) *ui.Pill {
	if !completed(status) {
		if status == "in_progress" {
			return ui.NewPill("running", ui.ToneNeutral)
		}
		return ui.NewPill("queued", ui.ToneMuted)
	}
	switch conclusion {
	case "success":
		return ui.NewPill("passed", ui.ToneOK)
	case "failure", "startup_failure":
		return ui.NewPill("failed", ui.ToneError)
	case "timed_out":
		return ui.NewPill("timed out", ui.ToneError)
	case "cancelled":
		return ui.NewPill("cancelled", ui.ToneMuted)
	case "skipped":
		return ui.NewPill("skipped", ui.ToneMuted)
	case "action_required":
		return ui.NewPill("needs action", ui.ToneAttention)
	case "neutral":
		return ui.NewPill("neutral", ui.ToneNeutral)
	default:
		return ui.NewPill(strings.ReplaceAll(conclusion, "_", " "), ui.ToneMuted)
	}
}

// runDuration is how long a finished run took, approximately: from its start to its
// last update.
func runDuration(r ghRun) time.Duration {
	start := r.StartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	if r.UpdatedAt.Before(start) {
		return 0
	}
	return r.UpdatedAt.Sub(start)
}

func jobDuration(j ghJob) time.Duration {
	if j.StartedAt.IsZero() || j.CompletedAt.Before(j.StartedAt) {
		return 0
	}
	return j.CompletedAt.Sub(j.StartedAt)
}

// failedStep names a job's first failed step, "" when none did.
func failedStep(j ghJob) string {
	for _, s := range j.Steps {
		if isFailure(s.Conclusion) {
			return s.Name
		}
	}
	return ""
}

// pickRun chooses the run the surface shows first: one in progress, else one that
// failed, else the newest.
func pickRun(runs []ghRun) int64 {
	for _, r := range runs {
		if !completed(r.Status) {
			return r.ID
		}
	}
	for _, r := range runs {
		if isFailure(r.Conclusion) {
			return r.ID
		}
	}
	if len(runs) > 0 {
		return runs[0].ID
	}
	return 0
}

func allCompleted(runs []ghRun) bool {
	for _, r := range runs {
		if !completed(r.Status) {
			return false
		}
	}
	return true
}

func findRun(runs []ghRun, id int64) (ghRun, bool) {
	for _, r := range runs {
		if r.ID == id {
			return r, true
		}
	}
	return ghRun{}, false
}

// actionsState summarizes a pull request's GitHub Actions checks for the flag.
type actionsState struct {
	// Failed is set when a check failed: Key identifies the failed run attempt (its run
	// id and failed job ids), Job names the first failed job.
	Failed bool
	Key    string
	Job    string
	RunID  int64
	// Passed: every Actions check finished and none failed.
	Passed bool
	Any    bool
}

func summarize(pc prChecks) actionsState {
	var st actionsState
	var failedJobs []string
	pending := false
	for _, c := range pc.Checks {
		if c.Provider != "github-actions" {
			continue
		}
		st.Any = true
		if !completed(strings.ToLower(c.Status)) {
			pending = true
			continue
		}
		if isFailure(strings.ToLower(c.Conclusion)) {
			run, job := runIDFromURL(c.URL)
			if !st.Failed {
				st.Failed, st.Job, st.RunID = true, c.Name, run
			}
			failedJobs = append(failedJobs, fmt.Sprintf("%d/%d", run, job))
		}
	}
	if st.Failed {
		sort.Strings(failedJobs)
		st.Key = strings.Join(failedJobs, ",")
	}
	st.Passed = st.Any && !st.Failed && !pending
	return st
}
