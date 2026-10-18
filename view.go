package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

// render sends the instance's view, then any log lines the host's buffer lacks.
func (s *runsSurface) render() error {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()
	s.mu.Lock()
	v, appends := s.view()
	s.mu.Unlock()
	if err := s.h.UpdateView(s.open.Instance, v); err != nil {
		s.h.Log().Error("view refused", "instance", s.open.Instance, "err", err)
		return err
	}
	if len(appends) > 0 {
		if err := s.h.AppendLogLines(s.open.Instance, "log-out", appends...); err != nil {
			s.h.Log().Warn("appending the log", "err", err)
		}
	}
	return nil
}

// view builds the whole view. Caller holds mu.
func (s *runsSurface) view() (ui.View, []pluginsdk.AppendLine) {
	v := ui.View{Header: s.header()}
	switch {
	case s.noPR:
		s.bufKey = ""
		v.Nodes = []ui.Node{{Key: "none", Empty: &ui.Empty{Glyph: ui.IconGlyph("git-pull-request", ui.ToneMuted),
			Title: "No pull request", Body: ui.Plain("This thread's branch has no pull request yet.")}}}
		return v, nil
	case !s.loaded && s.err != nil:
		s.bufKey = ""
		v.Nodes = []ui.Node{s.errorNotice()}
		return v, nil
	case !s.loaded:
		s.bufKey = ""
		v.Nodes = []ui.Node{ui.NewProgress("loading", "Loading workflow runs", nil)}
		return v, nil
	case len(s.runs) == 0:
		s.bufKey = ""
		v.Nodes = []ui.Node{{Key: "none", Empty: &ui.Empty{Glyph: ui.IconGlyph("workflow", ui.ToneMuted),
			Title:   "No workflow runs for " + short(s.pull.Head.SHA) + " yet",
			Body:    ui.Plain("Runs show here once GitHub Actions starts one for the pull request's head commit."),
			Actions: []ui.Button{ui.EventButton("empty-refresh", "Refresh", "refresh").WithIcon("refresh-cw")}}}}
		if s.err != nil {
			v.Nodes = append([]ui.Node{s.errorNotice()}, v.Nodes...)
		}
		return v, nil
	}

	v.Tabs = s.tabs()
	var body []ui.Node
	var appends []pluginsdk.AppendLine
	if bar, ok := s.actionBar(); ok {
		body = append(body, bar)
	}
	switch s.tab {
	case tabJobs:
		body = append(body, s.jobsNodes()...)
	case tabLog:
		var nodes []ui.Node
		nodes, appends = s.logNodes()
		body = append(body, nodes...)
	default:
		body = append(body, s.runsList())
	}
	if s.tab != tabLog {
		s.bufKey = ""
	}
	if s.err != nil {
		// The last data stays, dimmed, under what went wrong.
		v.Nodes = []ui.Node{s.errorNotice(), {Key: "stale", Section: &ui.Section{Stale: true, Children: body}}}
	} else {
		v.Nodes = body
	}
	return v, appends
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (s *runsSurface) header() *ui.Header {
	h := &ui.Header{Icon: "workflow", TabTitle: "Actions"}
	switch {
	case s.pull.Title != "":
		h.Title = fmt.Sprintf("#%d %s", s.pr.Number, s.pull.Title)
	case s.havePR:
		h.Title = s.pr.String()
	default:
		h.Title = "GitHub Actions"
	}
	if len(h.Title) > 120 {
		h.Title = h.Title[:117] + "…"
	}
	url := s.pull.HTMLURL
	if url == "" {
		url = s.pr.URL
	}
	run, ok := findRun(s.runs, s.selRun)
	if ok {
		h.Pill = pillOf(run.Status, run.Conclusion)
		spans := []ui.Span{ui.T(run.Name).AsStrong(), ui.T(fmt.Sprintf(" #%d", run.RunNumber))}
		if run.RunAttempt > 1 {
			spans = append(spans, ui.T(fmt.Sprintf(" (attempt %d)", run.RunAttempt)))
		}
		spans = append(spans, ui.T(" · "))
		if completed(run.Status) {
			spans = append(spans, ui.T("took "), ui.For(runDuration(run)), ui.T(" · "), ui.At(run.UpdatedAt, ui.TimeRelative))
		} else if run.Status == "in_progress" && !run.StartedAt.IsZero() {
			spans = append(spans, ui.T("running "), ui.At(run.StartedAt, ui.TimeElapsed))
		} else {
			since := run.StartedAt
			if since.IsZero() {
				since = run.CreatedAt
			}
			spans = append(spans, ui.T("queued "), ui.At(since, ui.TimeRelative))
		}
		h.Subline = ui.Spans(spans...)
		url = run.HTMLURL
	}
	if s.pull.Head.SHA != "" {
		h.Trailing = ui.Spans(ui.T(short(s.pull.Head.SHA)).AsMono())
	}
	if strings.HasPrefix(url, "https://") {
		h.Actions = append(h.Actions, ui.URLButton("hdr-open", "Open on GitHub", url).WithIcon("external-link").AsIconOnly())
	}
	if s.loaded || s.err != nil {
		h.Actions = append(h.Actions, ui.EventButton("hdr-refresh", "Refresh", "refresh").WithIcon("refresh-cw").AsIconOnly())
	}
	return h
}

func (s *runsSurface) tabs() *ui.Tabs {
	jobs := ui.NewTab(tabJobs, "Jobs")
	if n := len(s.failedJobs()); n > 0 {
		jobs.Badge = ui.NewPill(fmt.Sprintf("%d failed", n), ui.ToneError)
	}
	runs := ui.NewTab(tabRuns, "Runs")
	runs.Badge = ui.NewPill(strconv.Itoa(len(s.runs)), ui.ToneNeutral)
	return ui.NewTabs(s.tab, runs, jobs, ui.NewTab(tabLog, "Log"))
}

// actionBar holds what can be done to the selected run.
func (s *runsSurface) actionBar() (ui.Node, bool) {
	run, ok := findRun(s.runs, s.selRun)
	if !ok {
		return ui.Node{}, false
	}
	args := map[string]string{"run": strconv.FormatInt(run.ID, 10)}
	var b []ui.Button
	if completed(run.Status) {
		if _, _, ok := s.failureLocked(); ok && isFailure(run.Conclusion) {
			b = append(b, ui.EventButton("btn-send", "Send failure to Claude", "send-failure").WithStyle(ui.ButtonAttention).WithIcon("send"))
		}
		if isFailure(run.Conclusion) || run.Conclusion == "cancelled" {
			b = append(b, ui.CommandButton("btn-rerun-failed", "Rerun failed", "plugin.gh-actions.rerun-failed", args).WithStyle(ui.ButtonOutline).WithIcon("rotate-ccw"))
		}
		b = append(b, ui.CommandButton("btn-rerun", "Rerun all", "plugin.gh-actions.rerun", args).WithIcon("refresh-cw"))
	} else {
		b = append(b, ui.CommandButton("btn-cancel", "Cancel", "plugin.gh-actions.cancel", args).WithStyle(ui.ButtonDestructive).WithIcon("square"))
	}
	return ui.NewActions("bar", b...), true
}

func (s *runsSurface) runsList() ui.Node {
	rows := make([]ui.Row, 0, len(s.runs))
	for _, r := range s.runs {
		meta := fmt.Sprintf("#%d", r.RunNumber)
		if r.RunAttempt > 1 {
			meta += fmt.Sprintf(" · attempt %d", r.RunAttempt)
		}
		row := ui.Row{
			Key:       fmt.Sprintf("run-%d", r.ID),
			Glyph:     ui.StatusGlyph(statusOf(r.Status, r.Conclusion)),
			Title:     r.Name,
			TitleMeta: ui.Plain(meta),
			Pill:      pillOf(r.Status, r.Conclusion),
			Action:    ui.EventAction("select-run", strconv.FormatInt(r.ID, 10)),
			Selected:  r.ID == s.selRun,
		}
		switch {
		case completed(r.Status):
			row.Line2 = ui.Spans(ui.T(r.Event+" · "), ui.At(r.UpdatedAt, ui.TimeRelative))
			row.Trailing = ui.Spans(ui.For(runDuration(r)))
		case !r.StartedAt.IsZero() && r.Status == "in_progress":
			row.Line2 = ui.Spans(ui.T(r.Event + " · running"))
			row.Trailing = ui.Spans(ui.At(r.StartedAt, ui.TimeElapsed))
		default:
			row.Line2 = ui.Spans(ui.T(r.Event+" · queued "), ui.At(r.CreatedAt, ui.TimeRelative))
		}
		rows = append(rows, row)
	}
	return ui.Node{Key: "run-list", List: &ui.List{Rows: rows, EmptyText: "No runs"}}
}

func (s *runsSurface) jobsNodes() []ui.Node {
	if s.jobsFor != s.selRun {
		return []ui.Node{ui.NewProgress("jobs-loading", "Loading jobs", nil)}
	}
	counts := map[string]int{}
	rows := make([]ui.Row, 0, len(s.jobs))
	for _, j := range s.jobs {
		row := ui.Row{
			Key:      fmt.Sprintf("job-%d", j.ID),
			Glyph:    ui.StatusGlyph(statusOf(j.Status, j.Conclusion)),
			Title:    j.Name,
			Action:   ui.EventAction("select-job", strconv.FormatInt(j.ID, 10)),
			Selected: j.ID == s.selJob,
		}
		switch {
		case completed(j.Status) && isFailure(j.Conclusion):
			counts["failed"]++
			row.Pill = pillOf(j.Status, j.Conclusion)
			if step := failedStep(j); step != "" {
				row.Line2 = ui.Spans(ui.T("failed at "), ui.T(step).WithTone(ui.ToneError))
			}
			row.Trailing = ui.Spans(ui.For(jobDuration(j)))
		case completed(j.Status):
			word := strings.ReplaceAll(j.Conclusion, "_", " ")
			if j.Conclusion == "success" {
				word = "passed"
			} else {
				row.Line2 = ui.Plain(word)
			}
			counts[word]++
			if d := jobDuration(j); d > 0 {
				row.Trailing = ui.Spans(ui.For(d))
			}
		case j.Status == "in_progress":
			counts["running"]++
			if step := currentStep(j); step != "" {
				row.Line2 = ui.Spans(ui.T("running "), ui.T(step))
			}
			if !j.StartedAt.IsZero() {
				row.Trailing = ui.Spans(ui.At(j.StartedAt, ui.TimeElapsed))
			}
		default:
			counts["queued"]++
			row.Line2 = ui.Plain("queued")
		}
		rows = append(rows, row)
	}
	var summary []ui.Span
	for _, word := range []string{"failed", "running", "queued", "passed", "skipped", "cancelled", "neutral", "action required"} {
		if counts[word] == 0 {
			continue
		}
		sep := ""
		if len(summary) > 0 {
			sep = " · "
		}
		sp := ui.T(fmt.Sprintf("%s%d %s", sep, counts[word], word))
		if word == "failed" {
			sp = sp.WithTone(ui.ToneError)
		}
		summary = append(summary, sp)
	}
	if len(summary) == 0 {
		summary = []ui.Span{ui.T("No jobs yet")}
	}
	return []ui.Node{
		{Key: "jobs-summary", Text: &ui.Text{Content: ui.Spans(summary...), Size: ui.TextSm}},
		{Key: "job-list", List: &ui.List{Rows: rows, EmptyText: "No jobs yet"}},
	}
}

func currentStep(j ghJob) string {
	for _, st := range j.Steps {
		if st.Status == "in_progress" {
			return st.Name
		}
	}
	return ""
}

// logNodes is the Log tab: the failed steps' logs of the selected job, or of every
// failed job of the run. The log node's lines go once; later polls append new jobs'
// lines instead of re-sending them. Caller holds mu.
func (s *runsSurface) logNodes() ([]ui.Node, []pluginsdk.AppendLine) {
	if s.jobsFor != s.selRun {
		s.bufKey = ""
		return []ui.Node{ui.NewProgress("log-loading", "Loading jobs", nil)}, nil
	}
	run, _ := findRun(s.runs, s.selRun)
	var nodes []ui.Node
	var jobs []ghJob
	title := "Failed steps"
	if j, ok := s.job(s.selJob); ok {
		if !(completed(j.Status) && isFailure(j.Conclusion)) {
			s.bufKey = ""
			body := j.Name + " did not fail; this tab shows failed steps' logs."
			if !completed(j.Status) {
				body = j.Name + " is still running; its log shows here if a step fails."
			}
			return []ui.Node{{Key: "log-none", Empty: &ui.Empty{Glyph: ui.StatusGlyph(statusOf(j.Status, j.Conclusion)),
				Title: j.Name, Body: ui.Plain(body),
				Actions: []ui.Button{ui.EventButton("log-all", "All failed jobs", "all-failed")}}}}, nil
		}
		jobs = []ghJob{j}
		title = j.Name
		if st := failedStep(j); st != "" {
			title += " · " + st
		}
	} else {
		jobs = s.failedJobs()
		if len(jobs) > 1 {
			chips := make([]ui.Chip, 0, len(jobs))
			for _, j := range jobs {
				chips = append(chips, ui.Chip{Key: fmt.Sprintf("chip-%d", j.ID), Label: clip(j.Name, 32), Status: ui.StatusFailed,
					Meta: ui.Spans(ui.For(jobDuration(j))), Action: ui.EventAction("select-job", strconv.FormatInt(j.ID, 10))})
			}
			nodes = append(nodes, ui.Node{Key: "log-jobs", Chips: &ui.Chips{Chips: chips}})
		} else if len(jobs) == 1 {
			title = jobs[0].Name
			if st := failedStep(jobs[0]); st != "" {
				title += " · " + st
			}
		}
	}
	if len(jobs) == 0 {
		s.bufKey = ""
		e := &ui.Empty{Glyph: ui.StatusGlyph(statusOf(run.Status, run.Conclusion)), Title: "Nothing failed in this run"}
		if !completed(run.Status) {
			e.Title, e.Body = "Nothing failed yet", ui.Plain("Failed steps' logs show here as jobs finish.")
		}
		return append(nodes, ui.Node{Key: "log-none", Empty: e}), nil
	}
	// Only jobs whose log arrived; the rest show as an error or a wait.
	var ready []int64
	var lines []ui.LogLine
	truncated := false
	for _, j := range jobs {
		ls, ok := s.logs[j.ID]
		if !ok {
			continue
		}
		ready = append(ready, j.ID)
		lines = append(lines, jobLines(j, ls, len(jobs) > 1)...)
		truncated = truncated || len(ls) >= logTail
	}
	for _, j := range jobs {
		if msg, bad := s.logErrs[j.ID]; bad && s.logs[j.ID] == nil {
			nodes = append(nodes, ui.Node{Key: fmt.Sprintf("logerr-%d", j.ID), Notice: &ui.Notice{Tone: ui.ToneError,
				Title: "Could not read " + clip(j.Name, 60) + "'s log", Detail: clip(msg, 2000)}})
		}
	}
	if len(ready) == 0 {
		s.bufKey = ""
		return append(nodes, ui.NewProgress("log-loading", "Fetching the failed step's log", nil)), nil
	}
	key := fmt.Sprintf("%d/%d", s.selRun, s.selJob)
	logNode := &ui.Log{Title: clip(title, 200), Rows: 24, Wrap: true, Truncated: truncated}
	var appends []pluginsdk.AppendLine
	if s.bufKey != key {
		logNode.Lines = capLines(lines)
		s.bufKey, s.bufJobs = key, ready
	} else {
		for _, j := range jobs {
			if ls, ok := s.logs[j.ID]; ok && !contains(s.bufJobs, j.ID) {
				for _, l := range jobLines(j, ls, true) {
					appends = append(appends, pluginsdk.AppendLine{Text: l.Text, Tone: string(l.Tone)})
				}
				s.bufJobs = append(s.bufJobs, j.ID)
			}
		}
	}
	return append(nodes, ui.Node{Key: "log-out", Log: logNode}), appends
}

func contains(xs []int64, x int64) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// jobLines turns a job's failed log into toned lines, with a heading line when several
// jobs share the log.
func jobLines(j ghJob, ls []logLine, heading bool) []ui.LogLine {
	out := make([]ui.LogLine, 0, len(ls)+1)
	if heading {
		step := failedStep(j)
		if step == "" && len(ls) > 0 {
			step = ls[0].Step
		}
		out = append(out, ui.LogLine{Text: "── " + j.Name + " · " + step + " ──", Tone: ui.ToneMuted})
	}
	for _, l := range ls {
		text, tone := toneOf(l.Text)
		out = append(out, ui.LogLine{Text: clip(text, maxLogLine), Tone: tone})
	}
	return out
}

// toneOf colours a log line: GitHub's ##[error] and ##[warning] markers, Go's and most
// tools' failure words.
func toneOf(text string) (string, ui.Tone) {
	switch {
	case strings.HasPrefix(text, "##[error]"):
		return "Error: " + strings.TrimPrefix(text, "##[error]"), ui.ToneError
	case strings.HasPrefix(text, "##[warning]"):
		return "Warning: " + strings.TrimPrefix(text, "##[warning]"), ui.ToneAttention
	case strings.HasPrefix(text, "##[group]"):
		return strings.TrimPrefix(text, "##[group]"), ui.ToneMuted
	case strings.HasPrefix(text, "##["):
		if i := strings.IndexByte(text, ']'); i > 0 {
			return text[i+1:], ui.ToneMuted
		}
	}
	trimmed := strings.TrimSpace(text)
	low := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(trimmed, "--- FAIL"), strings.HasPrefix(trimmed, "FAIL"), strings.HasPrefix(low, "panic:"),
		strings.HasPrefix(low, "error"), strings.Contains(low, " error:"), strings.HasPrefix(trimmed, "✗"), strings.Contains(trimmed, "*** ["):
		return text, ui.ToneError
	case strings.HasPrefix(low, "warning"), strings.Contains(low, " warning:"):
		return text, ui.ToneAttention
	}
	return text, ui.ToneNeutral
}

// capLines keeps the log under the per-render limit, dropping its head.
func capLines(lines []ui.LogLine) []ui.LogLine {
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i].Text) + 1
		if total > ui.MaxLogText-1024 {
			return lines[i+1:]
		}
	}
	return lines
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// errorNotice says why the last fetch failed: no gh, not signed in, rate limited, or
// another error.
func (s *runsSurface) errorNotice() ui.Node {
	refresh := ui.EventButton("err-refresh", "Try again", "refresh").WithIcon("refresh-cw")
	n := &ui.Notice{Tone: ui.ToneError, Icon: "triangle-alert", Detail: clip(errText(s.err), ui.MaxNoticeDetail-8), Actions: []ui.Button{refresh}}
	stale := ""
	if s.loaded {
		stale = " Showing what was fetched " + ago(s.a.now().Sub(s.fetched)) + "."
	}
	switch kindOf(s.err) {
	case errNoGH:
		n.Title, n.Body = "GitHub CLI not found", ui.Plain("Install gh (brew install gh), sign in with `gh auth login`, then try again.")
		n.Tone, n.Icon = ui.ToneAttention, "terminal"
	case errAuth:
		n.Title, n.Body = "Sign in to GitHub", ui.Plain("gh is not signed in. Run `gh auth login` in a terminal, then try again."+stale)
		n.Tone, n.Icon = ui.ToneAttention, "key-round"
	case errRateLimit:
		n.Title, n.Body = "GitHub rate limit reached", ui.Plain("Trying again in a few minutes."+stale)
		n.Tone, n.Icon = ui.ToneAttention, "clock"
	default:
		n.Title, n.Body = "Could not read the runs from GitHub", ui.Plain("Trying again soon."+stale)
	}
	return ui.Node{Key: "error", Notice: n}
}

func errText(err error) string {
	var ge *ghError
	if errors.As(err, &ge) && strings.TrimSpace(ge.Output) != "" {
		return strings.TrimSpace(ge.Output)
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
}
