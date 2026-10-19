package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// The run-finished wake source (plugin.json's wakes): an agent's thread subscribes with
// `code-foundry thread subscribe --plugin gh-actions/run-finished [--arg run=<id>]` (pr
// defaults to the thread's pull request) and is woken when a workflow run on the pull
// request's head completes. The plugin polls each subscribed pull request's runs while
// it has a subscription (often while a run is going, rarely once all finished; an open
// panel's fetches and Code Foundry's pullRequestChanged events for a push or new checks
// count too), and fires once per
// run attempt that completes after the subscription was armed, with {pr, run} as the
// match. The host sends every live subscription again after each start, which re-arms.

const wakeRunFinished = "run-finished"

// maxWakeFailedJobs bounds the failed jobs a fire's payload lists.
const maxWakeFailedJobs = 20

// runWakes keeps the run-finished subscriptions and a poller per pull request.
type runWakes struct {
	a *app

	mu      sync.Mutex
	h       *pluginsdk.Host
	subs    map[string]string   // subscription → pull request key
	prs     map[string]*prWatch // pull request key → its poller
	stopped bool
}

// prWatch is one subscribed pull request: the run attempts already seen completed, so
// a fire is for one that completed while it was watched. Guarded by runWakes.mu.
type prWatch struct {
	key    string
	pr     prRef
	cancel context.CancelFunc
	kick   chan struct{}

	based bool            // the first observation is in: done is the baseline
	head  string          // the head commit done is for
	done  map[string]bool // "<run>/<attempt>" seen completed
}

func newRunWakes(a *app) *runWakes {
	return &runWakes{a: a, subs: map[string]string{}, prs: map[string]*prWatch{}}
}

// prKey is a pull request as the pr wake arg names it: "owner/name#123", lower case.
func prKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// parsePRArg reads a pr wake arg, "owner/name#123".
func parsePRArg(s string) (prRef, bool) {
	repo, num, ok := strings.Cut(strings.TrimSpace(s), "#")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n <= 0 || strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return prRef{}, false
	}
	return prRef{Repo: repo, Number: n, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)}, true
}

// runHint is the line that subscribes to the next finished run of a pull request (of
// one run when run is not 0).
func runHint(h *pluginsdk.Host, key string, run int64) string {
	args := map[string]string{"pr": key}
	if run != 0 {
		args["run"] = strconv.FormatInt(run, 10)
	}
	return h.WakeHint(wakeRunFinished, args)
}

// subscribed arms a subscription: the same id again (a restart's re-arm) is the same one.
func (w *runWakes) subscribed(h *pluginsdk.Host, s pluginsdk.WakeSubscription) {
	if s.Source != wakeRunFinished {
		h.Log().Warn("subscription to an unknown wake source", "source", s.Source, "subscription", s.Subscription)
		return
	}
	pr, ok := parsePRArg(s.Args["pr"])
	if !ok {
		h.Log().Warn("run-finished: bad pr", "pr", s.Args["pr"], "subscription", s.Subscription)
		return
	}
	key := prKey(pr.Repo, pr.Number)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.h = h
	if w.subs[s.Subscription] == key {
		return
	}
	w.dropLocked(s.Subscription)
	w.subs[s.Subscription] = key
	if p, ok := w.prs[key]; ok {
		p.poke()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &prWatch{key: key, pr: pr, cancel: cancel, kick: make(chan struct{}, 1), done: map[string]bool{}}
	w.prs[key] = p
	go w.loop(ctx, p)
	h.Log().Info("run-finished armed", "pr", key, "subscription", s.Subscription)
}

// unsubscribed disarms a subscription; the pull request's poller stops with its last one.
func (w *runWakes) unsubscribed(u pluginsdk.WakeUnsubscribed) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropLocked(u.Subscription)
}

func (w *runWakes) dropLocked(sub string) {
	key, ok := w.subs[sub]
	if !ok {
		return
	}
	delete(w.subs, sub)
	for _, k := range w.subs {
		if k == key {
			return
		}
	}
	if p, ok := w.prs[key]; ok {
		p.cancel()
		delete(w.prs, key)
		if w.h != nil {
			w.h.Log().Info("run-finished stopped", "pr", key)
		}
	}
}

// watching says whether a pull request has a run-finished poller (tests).
func (w *runWakes) watching(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prs[key] != nil
}

// stop ends every poller (the plugin is stopping).
func (w *runWakes) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	for k, p := range w.prs {
		p.cancel()
		delete(w.prs, k)
	}
}

// poke makes a pull request's poller fetch now (its checks changed, a rerun started).
func (w *runWakes) poke(key string) {
	w.mu.Lock()
	p := w.prs[key]
	w.mu.Unlock()
	if p != nil {
		p.poke()
	}
}

func (p *prWatch) poke() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (w *runWakes) loop(ctx context.Context, p *prWatch) {
	for {
		d := w.poll(ctx, p)
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
		case <-p.kick:
		case <-t.C:
		}
		t.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}

// poll reads the pull request's head and its runs, fires for the runs that finished,
// and says when to poll again: often while a run is going or none started yet, rarely
// once every run finished (a new push or a rerun is also seen through a poke).
func (w *runWakes) poll(ctx context.Context, p *prWatch) time.Duration {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	pull, err := w.a.gh.pull(cctx, p.pr.Repo, p.pr.Number)
	var runs []ghRun
	if err == nil {
		runs, err = w.a.gh.runs(cctx, p.pr.Repo, pull.Head.SHA)
	}
	if err != nil {
		if ctx.Err() == nil {
			w.log().Warn("run-finished: reading the runs", "pr", p.key, "err", err)
		}
		if kindOf(err) == errRateLimit {
			return w.a.rateLimitPoll
		}
		return w.a.wakePoll
	}
	w.observe(ctx, p.key, pull.Head.SHA, runs)
	next := w.a.wakeIdlePoll
	if len(runs) == 0 || !allCompleted(runs) {
		next = w.a.wakePoll
	}
	running := 0
	for _, r := range runs {
		if !completed(r.Status) {
			running++
		}
	}
	w.log().Debug("run-finished polled", "pr", p.key, "head", short(pull.Head.SHA), "runs", len(runs), "running", running, "next", next.String())
	return next
}

func (w *runWakes) log() *slog.Logger {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.h == nil {
		return slog.New(slog.DiscardHandler)
	}
	return w.h.Log()
}

// observe takes a pull request's runs on its head commit (from the poller or an open
// panel's fetch) and fires for every run attempt that completed since the last
// observation. The first observation is the baseline: what already finished is not
// news. Nothing happens for a pull request without a subscription.
func (w *runWakes) observe(ctx context.Context, key, head string, runs []ghRun) {
	w.mu.Lock()
	p := w.prs[key]
	if p == nil || w.h == nil {
		w.mu.Unlock()
		return
	}
	if p.head != head {
		p.head, p.done = head, map[string]bool{}
	}
	var fire []ghRun
	running := 0
	for _, r := range runs {
		if !completed(r.Status) {
			running++
			continue
		}
		k := fmt.Sprintf("%d/%d", r.ID, max(r.RunAttempt, 1))
		if p.done[k] {
			continue
		}
		p.done[k] = true
		if p.based {
			fire = append(fire, r)
		}
	}
	p.based = true
	h := w.h
	w.mu.Unlock()
	sort.SliceStable(fire, func(i, j int) bool { return fire[i].UpdatedAt.Before(fire[j].UpdatedAt) })
	for _, r := range fire {
		w.fire(ctx, h, p.pr, key, r, running)
	}
}

// fire fires run-finished for one completed run.
func (w *runWakes) fire(ctx context.Context, h *pluginsdk.Host, pr prRef, key string, r ghRun, running int) {
	failed := []map[string]any{}
	if isFailure(r.Conclusion) {
		jobs, err := w.a.gh.jobs(ctx, pr.Repo, r.ID)
		if err != nil {
			h.Log().Warn("run-finished: reading the failed jobs", "run", r.ID, "err", err)
		}
		for _, j := range jobs {
			if completed(j.Status) && isFailure(j.Conclusion) && len(failed) < maxWakeFailedJobs {
				failed = append(failed, map[string]any{"id": j.ID, "name": clip(j.Name, 100), "step": clip(failedStep(j), 100), "url": j.HTMLURL})
			}
		}
	}
	title := runTitle(r)
	payload := map[string]any{
		"pr": key, "run": r.ID, "name": clip(r.Name, 100), "number": r.RunNumber, "attempt": max(r.RunAttempt, 1),
		"event": r.Event, "status": r.Status, "conclusion": r.Conclusion, "url": r.HTMLURL, "head_sha": r.HeadSHA,
		"failed_jobs": failed, "still_running": running,
	}
	n, err := h.FireWake(ctx, wakeRunFinished, map[string]string{"pr": key, "run": strconv.FormatInt(r.ID, 10)}, title, payload)
	if err != nil {
		h.Log().Warn("run-finished: fire", "pr", key, "run", r.ID, "err", err)
		return
	}
	h.Log().Info("run-finished fired", "pr", key, "run", r.ID, "title", title, "woke", n)
}

// runTitle is a finished run in a few words: "ci #507 passed", "ci #494 failed (attempt 2)".
func runTitle(r ghRun) string {
	name := r.Name
	if name == "" {
		name = "run " + strconv.FormatInt(r.ID, 10)
	}
	t := fmt.Sprintf("%s #%d %s", clip(name, 120), r.RunNumber, conclusionWord(r.Conclusion))
	if r.RunAttempt > 1 {
		t += fmt.Sprintf(" (attempt %d)", r.RunAttempt)
	}
	return t
}

// conclusionWord is a finished run's or job's conclusion as a word or two.
func conclusionWord(c string) string {
	switch c {
	case "success":
		return "passed"
	case "failure", "startup_failure":
		return "failed"
	case "action_required":
		return "needs action"
	case "":
		return "finished"
	}
	return strings.ReplaceAll(c, "_", " ")
}
