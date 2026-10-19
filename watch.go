package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// watcher raises the thread flag: it follows the thread list and Code Foundry's
// gh.pullRequestChanged events, asks Code Foundry for the checks of each thread whose
// pull request changed (pr.checks), and flags a thread when one of its pull request's
// Actions runs failed, once per failure, until a later run passes. Nothing is read on a
// timer: a thread is checked when the thread list first shows it or its branch or
// linked pull requests change, when a pull request event names its pull request or
// branch, and every thread once when the host resends the gh snapshot.
type watcher struct {
	a *app
	h *pluginsdk.Host

	ctx    context.Context
	cancel context.CancelFunc
	kick   chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	threads map[string]threadInfo
	dirty   map[string]bool
	all     bool
	prOf    map[string]string // thread → prKey of the pull request pr.checks last answered with
	closed  map[string]bool   // prKey → merged or closed, as the last event about it said
	// notBefore: thread → when a run it followed (follow.go) ended; its next checks
	// answer must be fetched after it.
	notBefore map[string]time.Time

	// Owned by the worker goroutine.
	state   flagState
	prints  map[string]string        // thread → fingerprint of its checks, to wake surfaces
	failing map[string]bool          // thread → the gh-actions.failing fact sent
	live    map[string]bool          // thread → a flag this plugin raised may still be up
	backoff map[string]time.Duration // thread → the delay before re-reading checks that failed to load
	stale   map[string]int           // thread → answers older than its notBefore so far
}

// threadInfo is what the thread list says about a thread that can change its pull
// request.
type threadInfo struct {
	branch string
	prs    string // its linked pull requests' prKeys, sorted, comma-separated
}

// flagState is kept in the data dir so a restart does not flag the same failure again.
type flagState struct {
	// Flagged maps a thread to the failure it was flagged for (run and failed job ids).
	Flagged map[string]string `json:"flagged"`
}

func newWatcher(a *app, h *pluginsdk.Host) *watcher {
	return &watcher{
		a: a, h: h,
		kick:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		threads:   map[string]threadInfo{},
		dirty:     map[string]bool{},
		prOf:      map[string]string{},
		closed:    map[string]bool{},
		notBefore: map[string]time.Time{},
		state:     flagState{Flagged: map[string]string{}},
		prints:    map[string]string{},
		failing:   map[string]bool{},
		live:      map[string]bool{},
		backoff:   map[string]time.Duration{},
		stale:     map[string]int{},
	}
}

func (w *watcher) statePath() string { return filepath.Join(w.h.DataDir(), "flags.json") }

func (w *watcher) start(ctx context.Context) error {
	if data, err := os.ReadFile(w.statePath()); err == nil {
		if err := json.Unmarshal(data, &w.state); err != nil || w.state.Flagged == nil {
			w.state = flagState{Flagged: map[string]string{}}
		}
	}
	// A live flag the state does not know (a lost state file) is cleared by the next
	// passing run and replaced by the next failure.
	for _, f := range w.h.Flags() {
		if _, ok := w.state.Flagged[f.ThreadID]; !ok {
			w.state.Flagged[f.ThreadID] = "?"
		}
		if f.Level != pluginsdk.FlagNone {
			w.live[f.ThreadID] = true
		}
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	go w.run()
	if _, err := w.h.Subscribe(ctx, "threads", "gh"); err != nil {
		w.h.Log().Error("subscribing to threads and gh", "err", err)
		// Without events there is nothing to flag; the surface and commands still work.
	}
	return nil
}

func (w *watcher) stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
}

// threadEvent is the part of a threadList event the watcher reads.
type threadEvent struct {
	ID                 string `json:"id"`
	Branch             string `json:"branch"` // the branch Code Foundry made its worktree on, else ""
	LinkedPullRequests []struct {
		Slug   string `json:"slug"`
		Number int    `json:"number"`
	} `json:"linkedPullRequests"`
}

func (t threadEvent) info() threadInfo {
	keys := make([]string, 0, len(t.LinkedPullRequests))
	for _, p := range t.LinkedPullRequests {
		keys = append(keys, prKey(p.Slug, p.Number))
	}
	sort.Strings(keys)
	return threadInfo{branch: t.Branch, prs: strings.Join(keys, ",")}
}

// prChanged is the part of a gh.pullRequestChanged event the watcher reads.
type prChanged struct {
	RepoSlug    string `json:"repoSlug"`
	Number      int    `json:"number"`
	HeadRef     string `json:"headRef"`
	PullRequest struct {
		State string `json:"state"`
	} `json:"pullRequest"`
	Changes []struct {
		Kind string `json:"kind"`
	} `json:"changes"`
}

// Change kinds, protojson names.
const (
	kindAppeared   = "PULL_REQUEST_CHANGE_KIND_APPEARED"
	kindMerged     = "PULL_REQUEST_CHANGE_KIND_MERGED"
	kindClosed     = "PULL_REQUEST_CHANGE_KIND_CLOSED"
	kindReopened   = "PULL_REQUEST_CHANGE_KIND_REOPENED"
	kindHeadPushed = "PULL_REQUEST_CHANGE_KIND_HEAD_PUSHED"
	kindChecks     = "PULL_REQUEST_CHANGE_KIND_CHECKS"
)

// has says whether the event carries one of kinds.
func (e prChanged) has(kinds ...string) bool {
	for _, c := range e.Changes {
		for _, k := range kinds {
			if c.Kind == k {
				return true
			}
		}
	}
	return false
}

// flagRelevant: the change can move the flag. A new head or check rollup can raise or
// clear it, a pull request appearing or reopening can give a thread one, and a merged
// or closed one lowers it. Reviews, comments, drafts and titles cannot.
func (e prChanged) flagRelevant() bool {
	return e.has(kindChecks, kindHeadPushed, kindAppeared, kindMerged, kindClosed, kindReopened)
}

// isClosed: the pull request after the change is merged or closed.
func (e prChanged) isClosed() bool {
	return e.PullRequest.State == "PULL_REQUEST_STATE_MERGED" || e.PullRequest.State == "PULL_REQUEST_STATE_CLOSED"
}

// ghEvent is the part of the events the watcher reads.
type ghEvent struct {
	ThreadList *struct {
		Snapshot *struct {
			Threads []threadEvent `json:"threads"`
		} `json:"snapshot"`
		Updated   *threadEvent `json:"updated"`
		RemovedID string       `json:"removedId"`
	} `json:"threadList"`
	GH *struct {
		PullRequestChanged *prChanged `json:"pullRequestChanged"`
	} `json:"gh"`
}

// onEvent runs on the notification goroutine: it only marks threads to check.
func (w *watcher) onEvent(ev pluginsdk.Event) {
	var e ghEvent
	if err := json.Unmarshal(ev.Event, &e); err != nil {
		return
	}
	var poke *prChanged
	w.mu.Lock()
	switch {
	case e.ThreadList != nil && e.ThreadList.Snapshot != nil:
		w.threads = map[string]threadInfo{}
		for _, t := range e.ThreadList.Snapshot.Threads {
			w.threads[t.ID] = t.info()
		}
		w.all = true
	case e.ThreadList != nil && e.ThreadList.Updated != nil:
		// Most updates are status and activity; only a new thread, or a new branch or
		// linked pull request, can change which pull request pr.checks answers with.
		t := e.ThreadList.Updated
		if old, ok := w.threads[t.ID]; ok && old == t.info() {
			w.mu.Unlock()
			return
		}
		w.threads[t.ID] = t.info()
		w.dirty[t.ID] = true
	case e.ThreadList != nil && e.ThreadList.RemovedID != "":
		delete(w.threads, e.ThreadList.RemovedID)
		delete(w.dirty, e.ThreadList.RemovedID)
		delete(w.prOf, e.ThreadList.RemovedID)
		w.mu.Unlock()
		return
	case e.GH != nil && ev.Snapshot:
		// The host resent the gh snapshot (the start, or this plugin fell behind and
		// lost events): any pull request may have changed meanwhile.
		w.all = true
	case e.GH != nil && e.GH.PullRequestChanged != nil:
		pr := e.GH.PullRequestChanged
		key := prKey(pr.RepoSlug, pr.Number)
		if pr.isClosed() {
			w.closed[key] = true
		} else {
			delete(w.closed, key)
		}
		if pr.has(kindChecks, kindHeadPushed) {
			poke = pr
		}
		n := 0
		if pr.flagRelevant() {
			n = w.markPR(key, pr.HeadRef, pr.has(kindAppeared, kindReopened))
		}
		w.mu.Unlock()
		if poke != nil { // the open panels and run-finished pollers of the pull request
			w.a.poke(poke.RepoSlug, poke.Number)
		}
		if n == 0 {
			return // no thread on it (or nothing the flag cares about)
		}
		w.h.Log().Debug("pull request changed", "pr", key, "threads", n)
		w.signal()
		return
	default:
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.signal()
}

// markPR marks the threads on a pull request dirty and returns how many: the ones
// pr.checks last answered it for, the ones that linked it, the ones on its head branch
// (the branch Code Foundry made their worktree on). A pull request that appeared or
// reopened may also be one of the threads without a pull request yet whose branch the
// thread list does not say (a thread in an existing checkout has none there): those
// are read too, which costs no GitHub request when they still have none. Called with
// mu held.
func (w *watcher) markPR(key, headRef string, opened bool) int {
	n := 0
	for id, t := range w.threads {
		hit := w.prOf[id] == key || headRef != "" && t.branch == headRef || opened && w.prOf[id] == ""
		for _, p := range strings.Split(t.prs, ",") {
			hit = hit || p == key
		}
		if hit {
			w.dirty[id] = true
			n++
		}
	}
	return n
}

func (w *watcher) signal() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// run checks the threads events marked, a burst of events settling first.
func (w *watcher) run() {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.kick:
		}
		select { // let a burst of events settle
		case <-w.ctx.Done():
			return
		case <-time.After(w.a.debounce):
		}
		var ids []string
		w.mu.Lock()
		if w.all {
			for id := range w.threads {
				ids = append(ids, id)
			}
		} else {
			for id := range w.dirty {
				if _, ok := w.threads[id]; ok {
					ids = append(ids, id)
				}
			}
		}
		w.all, w.dirty = false, map[string]bool{}
		w.mu.Unlock()
		sort.Strings(ids)
		for _, id := range ids {
			if w.ctx.Err() != nil {
				return
			}
			w.check(id)
		}
	}
}

// check reads one thread's checks and raises, keeps or clears its flag.
func (w *watcher) check(thread string) {
	ctx, cancel := context.WithTimeout(w.ctx, 20*time.Second)
	defer cancel()
	pc, err := threadChecks(ctx, w.h, thread)
	if err != nil {
		if pluginsdk.ErrorCode(err) == pluginsdk.CodeUnavailable { // no pull request (any more)
			delete(w.backoff, thread)
			w.mu.Lock()
			delete(w.prOf, thread)
			w.mu.Unlock()
			w.setFailing(thread, false)
			if _, ok := w.state.Flagged[thread]; ok {
				w.clear(ctx, thread)
			}
			return
		}
		w.h.Log().Warn("reading checks", "thread", thread, "err", err)
		w.later(thread)
		return
	}
	if pc.LastError != "" {
		// The host's refresh failed and this is its older copy: the change that sent
		// us here may not be in it. Read again later; no event will say when.
		w.h.Log().Warn("reading checks: refresh failed", "thread", thread, "err", pc.LastError)
		w.later(thread)
	} else {
		delete(w.backoff, thread)
	}
	if !w.fresh(thread, pc) {
		return // the host's copy predates the run this thread waits on
	}
	key := prKey(pc.Repo, pc.Number)
	w.mu.Lock()
	w.prOf[thread] = key
	closed := w.closed[key]
	w.mu.Unlock()
	if fp := fingerprint(pc); w.prints[thread] != fp {
		if w.prints[thread] != "" {
			w.a.poke(pc.Repo, pc.Number)
		}
		w.prints[thread] = fp
	}
	if closed {
		// A merged or closed pull request's failure is no longer the thread's to fix:
		// lower the flag, but keep the failure's key so it is not raised again.
		w.setFailing(thread, false)
		if w.live[thread] {
			w.lower(ctx, thread)
		}
		return
	}
	st := summarize(pc)
	w.setFailing(thread, st.Failed)
	switch {
	case st.Failed:
		if w.state.Flagged[thread] == st.Key {
			return // already flagged for this failure (perhaps seen and cleared since)
		}
		err := w.h.SetFlag(ctx, pluginsdk.Flag{ThreadID: thread, Level: pluginsdk.FlagAttention,
			Reason: clip(st.Job+" failed", 80), UntilSeen: true})
		switch pluginsdk.ErrorCode(err) {
		case 0:
			w.h.Log().Info("flagged", "thread", thread, "job", st.Job, "run", strconv.FormatInt(st.RunID, 10))
			w.live[thread] = true
			w.state.Flagged[thread] = st.Key
			w.save()
		case pluginsdk.CodeRateLimited:
			w.retry(thread)
		default:
			w.h.Log().Warn("flagging", "thread", thread, "err", err)
		}
	case st.Passed:
		if _, ok := w.state.Flagged[thread]; ok {
			w.clear(ctx, thread)
		}
	}
}

// clear lowers a thread's flag and forgets its failure (a later run passed, or the
// thread has no pull request now).
func (w *watcher) clear(ctx context.Context, thread string) {
	if !w.lower(ctx, thread) {
		return
	}
	delete(w.state.Flagged, thread)
	w.save()
}

// lower clears a thread's flag on the host; false when that failed.
func (w *watcher) lower(ctx context.Context, thread string) bool {
	if err := w.h.SetFlag(ctx, pluginsdk.Flag{ThreadID: thread, Level: pluginsdk.FlagNone}); err != nil &&
		pluginsdk.ErrorCode(err) != pluginsdk.CodeNotFound && pluginsdk.ErrorCode(err) != pluginsdk.CodeUnavailable {
		w.h.Log().Warn("clearing the flag", "thread", thread, "err", err)
		return false
	}
	w.h.Log().Info("cleared", "thread", thread)
	delete(w.live, thread)
	return true
}

func (w *watcher) retry(thread string) {
	w.mu.Lock()
	w.dirty[thread] = true
	w.mu.Unlock()
	w.signal()
}

// later re-reads a thread's checks after a read that failed: the event that asked for
// it will not come again. The delay doubles from retryMin to retryMax and resets on a
// good read.
func (w *watcher) later(thread string) {
	d := w.backoff[thread]
	switch {
	case d == 0:
		d = w.a.retryMin
	case d < w.a.retryMax:
		d = min(2*d, w.a.retryMax)
	}
	w.backoff[thread] = d
	time.AfterFunc(d, func() {
		if w.ctx.Err() == nil {
			w.retry(thread)
		}
	})
}

// setFailing keeps the gh-actions.failing fact of a thread current.
func (w *watcher) setFailing(thread string, failing bool) {
	if w.failing[thread] == failing {
		return
	}
	w.failing[thread] = failing
	var v any = true
	if !failing {
		v = nil
	}
	_ = w.h.SetContext(pluginsdk.ContextParams{Key: "failing", Value: v, ThreadID: thread})
}

func (w *watcher) save() {
	data, err := json.Marshal(w.state)
	if err == nil {
		err = os.WriteFile(w.statePath(), data, 0o600)
	}
	if err != nil {
		w.h.Log().Warn("saving the flag state", "err", err)
	}
}

func fingerprint(pc prChecks) string {
	var b strings.Builder
	b.WriteString(pc.HeadSHA)
	for _, c := range pc.Checks {
		if c.Provider == "github-actions" {
			b.WriteString("|" + c.URL + "=" + c.Status + "/" + c.Conclusion)
		}
	}
	return b.String()
}
