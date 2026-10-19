package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

const (
	// Tab keys share the view's key namespace with nodes ("run-list", "job-list").
	tabRuns = "tab-runs"
	tabJobs = "tab-jobs"
	tabLog  = "tab-log"

	// logTail is how many lines of one job's failed steps the Log tab keeps.
	logTail = 200
	// maxLogLine cuts very long log lines.
	maxLogLine = 400
)

// runsSurface is one open instance of the runs surface: one pull request's runs.
type runsSurface struct {
	a    *app
	h    *pluginsdk.Host
	open pluginsdk.SurfaceOpen

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	renderMu sync.Mutex // one view (and its appends) at a time, in order

	mu      sync.Mutex
	pr      prRef
	havePR  bool
	noPR    bool
	pull    ghPull
	runs    []ghRun
	loaded  bool
	fetched time.Time
	err     error
	visible bool

	selRun  int64
	selJob  int64 // 0: every failed job of the selected run
	jobs    []ghJob
	jobsFor int64
	logs    map[int64][]logLine // failed steps' lines by job id
	logErrs map[int64]string
	tab     string

	// What the host's buffer of the log node holds: the node's key (one per run attempt
	// and job selection, so a new run never shows the last one's lines) and the jobs
	// whose lines are in it.
	bufKey  string
	bufJobs []int64
}

func (a *app) newRuns(_ context.Context, h *pluginsdk.Host, open pluginsdk.SurfaceOpen) (pluginsdk.Surface, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &runsSurface{
		a: a, h: h, open: open, ctx: ctx, cancel: cancel,
		wake:    make(chan struct{}, 1),
		logs:    map[int64][]logLine{},
		logErrs: map[int64]string{},
		tab:     tabRuns,
		visible: true,
	}
	if ref, ok := prFromFacts(open.Facts); ok {
		s.pr, s.havePR = ref, true
	}
	a.mu.Lock()
	a.surfaces[open.Instance] = s
	a.mu.Unlock()
	if err := s.render(); err != nil { // "Loading", so the host has a view at once
		cancel()
		return nil, err
	}
	go s.loop()
	return s, nil
}

func (s *runsSurface) isFor(repo string, number int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.havePR && strings.EqualFold(s.pr.Repo, repo) && s.pr.Number == number
}

func (s *runsSurface) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// loop fetches, renders, and waits for the next poll or a wake-up.
func (s *runsSurface) loop() {
	for {
		s.refresh()
		var tick <-chan time.Time
		var t *time.Timer
		if d := s.nextPoll(); d > 0 {
			t = time.NewTimer(d)
			tick = t.C
		}
		select {
		case <-s.ctx.Done():
		case <-s.wake:
		case <-tick:
		}
		if t != nil {
			t.Stop()
		}
		if s.ctx.Err() != nil {
			return
		}
	}
}

// nextPoll says when to fetch again: every poll while a run is in progress and someone
// looks, less often while hidden, never once every run finished (a rerun or a push
// wakes the surface through the flag watcher).
func (s *runsSurface) nextPoll() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.a
	switch {
	case s.err != nil && kindOf(s.err) == errRateLimit:
		return a.rateLimitPoll
	case s.err != nil && (kindOf(s.err) == errAuth || kindOf(s.err) == errNoGH):
		return 0 // waits for Refresh
	case s.err != nil:
		if s.visible {
			return a.poll
		}
		return a.hiddenPoll
	case s.noPR:
		return 0
	case !s.loaded || len(s.runs) == 0:
		if s.visible {
			return a.waitPoll
		}
		return 0
	case !allCompleted(s.runs) || s.jobsRunning():
		if s.visible {
			return a.poll
		}
		return a.hiddenPoll
	}
	return 0
}

// jobsRunning: the selected run's jobs are not all done (a rerun of some jobs). Caller
// holds mu.
func (s *runsSurface) jobsRunning() bool {
	if s.jobsFor != s.selRun {
		return false
	}
	for _, j := range s.jobs {
		if !completed(j.Status) {
			return true
		}
	}
	return false
}

func (s *runsSurface) refresh() {
	ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer cancel()
	err := s.fetch(ctx)
	if s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	if err != nil {
		s.h.Log().Warn("fetching runs", "instance", s.open.Instance, "err", err)
	}
	_ = s.render()
}

func (s *runsSurface) fetch(ctx context.Context) error {
	s.mu.Lock()
	pr, have := s.pr, s.havePR
	s.mu.Unlock()
	if !have {
		pc, err := threadChecks(ctx, s.h, s.open.Context.ThreadID)
		if pluginsdk.ErrorCode(err) == pluginsdk.CodeUnavailable {
			s.mu.Lock()
			s.noPR = true
			s.mu.Unlock()
			return nil
		}
		if err != nil {
			return err
		}
		pr = prRef{Repo: pc.Repo, Number: pc.Number, URL: pc.URL}
		s.mu.Lock()
		s.pr, s.havePR, s.noPR = pr, true, false
		s.mu.Unlock()
	}
	pull, err := s.a.gh.pull(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	runs, err := s.a.gh.runs(ctx, pr.Repo, pull.Head.SHA)
	if err != nil {
		return err
	}
	// A run-finished subscription to this pull request learns of a finished run as soon
	// as the panel sees it.
	s.a.wakes.observe(ctx, prKey(pr.Repo, pr.Number), pull.Head.SHA, runs)
	s.mu.Lock()
	if s.pull.Head.SHA != pull.Head.SHA { // a new head: start over
		s.selRun, s.selJob, s.jobs, s.jobsFor = 0, 0, nil, 0
		s.logs, s.logErrs, s.bufKey = map[int64][]logLine{}, map[int64]string{}, ""
	}
	s.pull, s.runs, s.loaded, s.fetched = pull, runs, true, s.a.now()
	if _, ok := findRun(runs, s.selRun); !ok {
		s.selRun, s.selJob = pickRun(runs), 0
	}
	sel := s.selRun
	s.mu.Unlock()
	if sel == 0 {
		return nil
	}
	_ = s.render() // the runs now, the jobs when they come
	jobs, err := s.a.gh.jobs(ctx, pr.Repo, sel)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.selRun == sel {
		s.jobs, s.jobsFor = jobs, sel
	}
	var need []int64
	for _, j := range jobs {
		if completed(j.Status) && isFailure(j.Conclusion) && s.logs[j.ID] == nil {
			need = append(need, j.ID)
		}
	}
	s.mu.Unlock()
	for _, id := range need {
		lines, err := s.a.gh.failedLog(ctx, pr.Repo, id)
		s.mu.Lock()
		if err != nil {
			s.logErrs[id] = err.Error()
		} else {
			delete(s.logErrs, id)
			s.logs[id] = tail(lines, logTail)
		}
		s.mu.Unlock()
	}
	return nil
}

func tail[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func (s *runsSurface) Visibility(visible bool) {
	s.mu.Lock()
	was := s.visible
	s.visible = visible
	s.mu.Unlock()
	if visible && !was {
		s.wakeUp()
	}
}

func (s *runsSurface) Close(pluginsdk.CloseReason) {
	s.cancel()
	s.a.mu.Lock()
	delete(s.a.surfaces, s.open.Instance)
	s.a.mu.Unlock()
}

// Act handles tabs, row and chip selection, refresh and sending the failure to Claude.
// Rerun and cancel are command buttons: the host runs those.
func (s *runsSurface) Act(ctx context.Context, act pluginsdk.SurfaceAct) error {
	switch act.Action {
	case "tab":
		switch act.Key {
		case tabRuns, tabJobs, tabLog:
		default:
			return fmt.Errorf("no tab %q", act.Key)
		}
		s.mu.Lock()
		s.tab = act.Key
		s.mu.Unlock()
		return s.render()
	case "select-run":
		id, err := strconv.ParseInt(act.Data, 10, 64)
		s.mu.Lock()
		if _, ok := findRun(s.runs, id); err != nil || !ok {
			s.mu.Unlock()
			return fmt.Errorf("no run %q on this commit", act.Data)
		}
		if s.selRun != id {
			s.selRun, s.selJob = id, 0
		}
		s.tab = tabJobs
		s.mu.Unlock()
		s.wakeUp()
		return s.render()
	case "select-job":
		id, err := strconv.ParseInt(act.Data, 10, 64)
		s.mu.Lock()
		if _, ok := s.job(id); err != nil || !ok {
			s.mu.Unlock()
			return fmt.Errorf("no job %q in this run", act.Data)
		}
		s.selJob, s.tab = id, tabLog
		s.mu.Unlock()
		return s.render()
	case "all-failed":
		s.mu.Lock()
		s.selJob, s.tab = 0, tabLog
		s.mu.Unlock()
		return s.render()
	case "refresh":
		s.wakeUp()
		return nil
	case "send-failure":
		// The view sends this as a command (plugin.gh-actions.send-failure), which the
		// host runs with the viewer's thread. An older view's event button lands here:
		// the acting panel's thread, else (a host without act.context) the opener's.
		thread := act.Context.ThreadID
		if thread == "" {
			thread = s.open.Context.ThreadID
		}
		return s.sendFailure(ctx, thread)
	}
	return fmt.Errorf("unknown action %q", act.Action)
}

// job finds a job of the selected run. Caller holds mu.
func (s *runsSurface) job(id int64) (ghJob, bool) {
	if s.jobsFor != s.selRun {
		return ghJob{}, false
	}
	for _, j := range s.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return ghJob{}, false
}

// sendFailure sends the selected run's failure to a thread.
func (s *runsSurface) sendFailure(ctx context.Context, thread string) error {
	s.mu.Lock()
	run, _ := findRun(s.runs, s.selRun)
	job, lines, ok := s.failureLocked()
	pr := s.pr
	s.mu.Unlock()
	if !ok {
		return errors.New("no failed step with a log in this run yet")
	}
	if thread == "" {
		return errors.New("no thread to send to")
	}
	if err := sendExcerpt(ctx, s.h, thread, pr, run, job, lines); err != nil {
		return err
	}
	_ = s.h.ToastIn(s.open.Instance, pluginsdk.ToastInfo, "Sent "+job.Name+"'s failure to Claude")
	return nil
}

// sendExcerpt sends a failed job's excerpt into a thread, ending with the line that
// waits for the pull request's next finished run.
func sendExcerpt(ctx context.Context, h *pluginsdk.Host, thread string, pr prRef, run ghRun, job ghJob, lines []logLine) error {
	text := failureExcerpt(run, job, lines, runHint(h, prKey(pr.Repo, pr.Number), 0))
	if _, err := h.Invoke(ctx, "thread.send", map[string]string{"text": text}, pluginsdk.UiContext{ThreadID: thread}); err != nil {
		return fmt.Errorf("sending the failure: %w", err)
	}
	return nil
}

// cachedFailure is a failed job of run and its log as an open surface of the pull
// request already has them: job, else the first failed job with a log.
func (s *runsSurface) cachedFailure(runID, jobID int64) (ghRun, ghJob, []logLine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := findRun(s.runs, runID)
	if !ok || s.jobsFor != runID {
		return ghRun{}, ghJob{}, nil, false
	}
	for _, j := range s.failedJobs() {
		if (jobID == 0 || j.ID == jobID) && s.logs[j.ID] != nil {
			return run, j, s.logs[j.ID], true
		}
	}
	return ghRun{}, ghJob{}, nil, false
}

// failureLocked picks the failed job to send: the selected one if it failed, else the
// first failed job with a log.
func (s *runsSurface) failureLocked() (ghJob, []logLine, bool) {
	if j, ok := s.job(s.selJob); ok && s.logs[j.ID] != nil {
		return j, s.logs[j.ID], true
	}
	for _, j := range s.failedJobs() {
		if s.logs[j.ID] != nil {
			return j, s.logs[j.ID], true
		}
	}
	return ghJob{}, nil, false
}

// failedJobs are the selected run's finished failed jobs. Caller holds mu.
func (s *runsSurface) failedJobs() []ghJob {
	if s.jobsFor != s.selRun {
		return nil
	}
	var out []ghJob
	for _, j := range s.jobs {
		if completed(j.Status) && isFailure(j.Conclusion) {
			out = append(out, j)
		}
	}
	return out
}

// failureExcerpt is the message "Send failure to Claude" sends: at most 4 KB with the
// workflow, job, step, the run's URL and up to 60 lines of the failed step's log, then
// hint, the line that waits for the next run. A failed step's last lines are often a
// summary (Go's "ok" lines after the one FAIL), so the excerpt keeps the lines that look
// like errors (with the two after each) and the last 20 lines, in order, "…" marking
// what was left out.
func failureExcerpt(run ghRun, job ghJob, lines []logLine, hint string) string {
	const limit = 4096
	step := failedStep(job)
	if step == "" && len(lines) > 0 {
		step = lines[len(lines)-1].Step
	}
	url := job.HTMLURL
	if url == "" {
		url = run.HTMLURL
	}
	head := fmt.Sprintf("The GitHub Actions run for this pull request failed. Please investigate: read the log below, find the cause, and tell me what went wrong (fix it if the cause is in this branch).\n\n"+
		"Workflow: %s (run #%d, attempt %d)\nJob: %s\nStep: %s\nRun: %s\n\nThe failed step's log (error lines and its last lines):\n```\n",
		run.Name, run.RunNumber, max(run.RunAttempt, 1), job.Name, step, url)
	foot := "```\n\nAfter pushing a fix or rerunning the job, wait for the run to finish instead of polling:\n" + hint
	body := excerptLines(lines, 60, 20)
	for len(body) > 0 && len(head)+len(strings.Join(body, "\n"))+1+len(foot) > limit {
		body = body[1:]
	}
	if len(body) == 0 {
		return truncate(head, limit-len(foot)) + foot
	}
	return head + strings.Join(body, "\n") + "\n" + foot
}

// excerptLines picks at most max lines: the last tailN, and before them the lines that
// look like errors with the two that follow each (the latest ones when there are too
// many), with "…" where lines were skipped.
func excerptLines(lines []logLine, maxLines, tailN int) []string {
	keep := make([]bool, len(lines))
	for i := max(0, len(lines)-tailN); i < len(lines); i++ {
		keep[i] = true
	}
	budget := maxLines - min(tailN, len(lines))
	for i := len(lines) - tailN - 1; i >= 0 && budget > 0; i-- {
		if _, tone := toneOf(lines[i].Text); tone != ui.ToneError {
			continue
		}
		for j := min(i+2, len(lines)-1); j >= i && budget > 0; j-- {
			if !keep[j] {
				keep[j] = true
				budget--
			}
		}
	}
	var out []string
	gap := false
	for i, l := range lines {
		if !keep[i] {
			gap = true
			continue
		}
		if gap {
			out = append(out, "…")
		}
		gap = false
		t, _ := toneOf(l.Text)
		if len(t) > 300 {
			t = t[:300] + "…"
		}
		out = append(out, t)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
