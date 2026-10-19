package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// target is the run a command acts on, of the thread's pull request.
type target struct {
	Thread string
	PR     prRef
	SHA    string
	Run    ghRun
}

// resolve finds the thread's pull request and the run a command acts on: the --run
// arg, else the run pick chooses from the head commit's runs (newest first).
func (a *app) resolve(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest, what string, pick func([]ghRun) (ghRun, bool)) (target, error) {
	var t target
	thread := req.Arg("thread")
	if thread == "" {
		thread = req.Context.ThreadID
	}
	if thread == "" {
		return t, pluginsdk.NewError(pluginsdk.CodeInvalidParams, "name a thread with --thread <id> (or run this from a thread)")
	}
	pc, err := threadChecks(ctx, h, thread)
	if err != nil {
		return t, err
	}
	t.Thread = thread
	t.PR = prRef{Repo: pc.Repo, Number: pc.Number, URL: pc.URL}
	if s := req.Arg("run"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return t, pluginsdk.NewError(pluginsdk.CodeInvalidParams, "run %q is not a run id", s)
		}
		t.Run = ghRun{ID: id, HTMLURL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", t.PR.Repo, id)}
		return t, nil
	}
	pull, err := a.gh.pull(ctx, t.PR.Repo, t.PR.Number)
	if err != nil {
		return t, err
	}
	t.SHA = pull.Head.SHA
	runs, err := a.gh.runs(ctx, t.PR.Repo, t.SHA)
	if err != nil {
		return t, err
	}
	run, ok := pick(runs)
	if !ok {
		return t, pluginsdk.NewError(pluginsdk.CodeUnavailable, "%s for %s at %s", what, t.PR, short(t.SHA))
	}
	t.Run = run
	return t, nil
}

func newestWhere(cond func(ghRun) bool) func([]ghRun) (ghRun, bool) {
	return func(runs []ghRun) (ghRun, bool) {
		for _, r := range runs {
			if cond(r) {
				return r, true
			}
		}
		return ghRun{}, false
	}
}

func runLabel(r ghRun) string {
	if r.Name == "" {
		return fmt.Sprintf("run %d", r.ID)
	}
	return fmt.Sprintf("%s #%d", r.Name, r.RunNumber)
}

// waitLine ends a message about a run with the line that waits for it to finish.
func waitLine(h *pluginsdk.Host, t target) string {
	return "\nWait for it to finish: " + runHint(h, prKey(t.PR.Repo, t.PR.Number), t.Run.ID)
}

func result(msg string, t target) (pluginsdk.CommandResult, error) {
	js, err := json.Marshal(map[string]any{"repo": t.PR.Repo, "number": t.PR.Number, "run": t.Run.ID, "url": t.Run.HTMLURL})
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	return pluginsdk.CommandResult{Message: msg, JSON: js}, nil
}

// rerunFailed reruns the failed jobs of the newest failed (or cancelled) run.
func (a *app) rerunFailed(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.resolve(ctx, h, req, "no failed run", newestWhere(func(r ghRun) bool {
		return completed(r.Status) && (isFailure(r.Conclusion) || r.Conclusion == "cancelled")
	}))
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	if err := a.gh.rerun(ctx, t.PR.Repo, t.Run.ID, true); err != nil {
		return pluginsdk.CommandResult{}, fmt.Errorf("rerunning %s: %w", runLabel(t.Run), err)
	}
	a.changed(t)
	return result("Rerunning the failed jobs of "+runLabel(t.Run)+waitLine(h, t), t)
}

// rerunAll reruns every job of the newest finished run.
func (a *app) rerunAll(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.resolve(ctx, h, req, "no finished run", newestWhere(func(r ghRun) bool { return completed(r.Status) }))
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	if err := a.gh.rerun(ctx, t.PR.Repo, t.Run.ID, false); err != nil {
		return pluginsdk.CommandResult{}, fmt.Errorf("rerunning %s: %w", runLabel(t.Run), err)
	}
	a.changed(t)
	return result("Rerunning every job of "+runLabel(t.Run)+waitLine(h, t), t)
}

// cancel cancels the newest run in progress (the manifest makes the host confirm).
func (a *app) cancel(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.resolve(ctx, h, req, "no run in progress", newestWhere(func(r ghRun) bool { return !completed(r.Status) }))
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	if err := a.gh.cancel(ctx, t.PR.Repo, t.Run.ID); err != nil {
		return pluginsdk.CommandResult{}, fmt.Errorf("cancelling %s: %w", runLabel(t.Run), err)
	}
	a.changed(t)
	return result("Cancelling "+runLabel(t.Run)+waitLine(h, t), t)
}

// open opens a run on GitHub: the one in progress, else the failed one, else the
// newest; the pull request's checks page when it has no runs.
func (a *app) open(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	t, err := a.resolve(ctx, h, req, "no run", func(runs []ghRun) (ghRun, bool) {
		if id := pickRun(runs); id != 0 {
			return findRun(runs, id)
		}
		return ghRun{}, true // no runs yet: the checks page
	})
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	url := t.Run.HTMLURL
	if url == "" {
		url = t.PR.URL + "/checks"
	}
	if _, err := h.Invoke(ctx, "view.open.url", map[string]string{"url": url}, req.Context); err != nil {
		return pluginsdk.CommandResult{}, fmt.Errorf("opening %s: %w", url, err)
	}
	t.Run.HTMLURL = url
	return result("Opened "+url, t)
}

// sendFailure sends a failed job's log excerpt to Claude in the thread: the newest
// failed run's (or --run's) first failed job with a log (or --job). The view's "Send
// failure to Claude" button runs it, so the host fills --thread with the thread the
// user is looking at, not the one that opened the shared pull request panel.
func (a *app) sendFailure(ctx context.Context, h *pluginsdk.Host, req pluginsdk.CommandRequest) (pluginsdk.CommandResult, error) {
	var jobID int64
	if s := req.Arg("job"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return pluginsdk.CommandResult{}, pluginsdk.NewError(pluginsdk.CodeInvalidParams, "job %q is not a job id", s)
		}
		jobID = id
	}
	t, err := a.resolve(ctx, h, req, "no failed run", newestWhere(func(r ghRun) bool {
		return completed(r.Status) && isFailure(r.Conclusion)
	}))
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	run, job, lines, err := a.failure(ctx, t, jobID)
	if err != nil {
		return pluginsdk.CommandResult{}, err
	}
	if err := sendExcerpt(ctx, h, t.Thread, t.PR, run, job, lines); err != nil {
		return pluginsdk.CommandResult{}, err
	}
	t.Run = run
	return result("Sent "+job.Name+"'s failure to Claude", t)
}

// failure finds a failed job of t.Run and its failed steps' log: from an open surface
// of the pull request when it has them, else from GitHub.
func (a *app) failure(ctx context.Context, t target, jobID int64) (ghRun, ghJob, []logLine, error) {
	a.mu.Lock()
	var open []*runsSurface
	for _, s := range a.surfaces {
		open = append(open, s)
	}
	a.mu.Unlock()
	for _, s := range open {
		if s.isFor(t.PR.Repo, t.PR.Number) {
			if run, job, lines, ok := s.cachedFailure(t.Run.ID, jobID); ok {
				return run, job, lines, nil
			}
		}
	}
	run := t.Run
	if run.Name == "" { // --run: only its id is known
		r, err := a.gh.run(ctx, t.PR.Repo, run.ID)
		if err != nil {
			return run, ghJob{}, nil, fmt.Errorf("reading run %d: %w", run.ID, err)
		}
		run = r
	}
	jobs, err := a.gh.jobs(ctx, t.PR.Repo, run.ID)
	if err != nil {
		return run, ghJob{}, nil, fmt.Errorf("reading the jobs of %s: %w", runLabel(run), err)
	}
	for _, j := range jobs {
		if jobID != 0 && j.ID != jobID {
			continue
		}
		if !completed(j.Status) || !isFailure(j.Conclusion) {
			if jobID != 0 {
				return run, ghJob{}, nil, pluginsdk.NewError(pluginsdk.CodeUnavailable, "job %s of %s did not fail", j.Name, runLabel(run))
			}
			continue
		}
		lines, err := a.gh.failedLog(ctx, t.PR.Repo, j.ID)
		if err != nil {
			return run, ghJob{}, nil, fmt.Errorf("reading %s's log: %w", j.Name, err)
		}
		if len(lines) == 0 {
			continue
		}
		return run, j, tail(lines, logTail), nil
	}
	if jobID != 0 {
		return run, ghJob{}, nil, pluginsdk.NewError(pluginsdk.CodeNotFound, "no job %d in %s", jobID, runLabel(run))
	}
	return run, ghJob{}, nil, pluginsdk.NewError(pluginsdk.CodeUnavailable, "no failed step with a log in %s", runLabel(run))
}

// changed wakes what shows the pull request's runs after a rerun or a cancel: its open
// surfaces now, the flag watcher on its next sweeps.
func (a *app) changed(t target) {
	a.poke(t.PR.Repo, t.PR.Number)
	if a.watch != nil {
		a.watch.expect(t.Thread)
	}
}
