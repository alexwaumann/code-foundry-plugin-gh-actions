package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// watcher raises the thread flag: it follows the thread list and GitHub polling
// events, asks Code Foundry for each thread's pull request checks (pr.checks, cached
// by the daemon), and flags a thread when one of its pull request's Actions runs
// failed, once per failure, until a later run passes.
type watcher struct {
	a *app
	h *pluginsdk.Host

	ctx    context.Context
	cancel context.CancelFunc
	kick   chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	threads map[string]bool
	dirty   map[string]bool
	all     bool

	// Owned by the worker goroutine.
	state   flagState
	prints  map[string]string // thread → fingerprint of its checks, to wake surfaces
	failing map[string]bool   // thread → the gh-actions.failing fact sent
}

// flagState is kept in the data dir so a restart does not flag the same failure again.
type flagState struct {
	// Flagged maps a thread to the failure it was flagged for (run and failed job ids).
	Flagged map[string]string `json:"flagged"`
}

func newWatcher(a *app, h *pluginsdk.Host) *watcher {
	return &watcher{
		a: a, h: h,
		kick:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		threads: map[string]bool{},
		dirty:   map[string]bool{},
		state:   flagState{Flagged: map[string]string{}},
		prints:  map[string]string{},
		failing: map[string]bool{},
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

// onEvent runs on the notification goroutine: it only marks threads to check.
func (w *watcher) onEvent(ev pluginsdk.Event) {
	var e struct {
		ThreadList *struct {
			Snapshot *struct {
				Threads []struct {
					ID string `json:"id"`
				} `json:"threads"`
			} `json:"snapshot"`
			Updated *struct {
				ID string `json:"id"`
			} `json:"updated"`
			RemovedID string `json:"removedId"`
		} `json:"threadList"`
		GH map[string]json.RawMessage `json:"gh"`
	}
	if err := json.Unmarshal(ev.Event, &e); err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case e.ThreadList != nil && e.ThreadList.Snapshot != nil:
		w.threads = map[string]bool{}
		for _, t := range e.ThreadList.Snapshot.Threads {
			w.threads[t.ID] = true
		}
		w.all = true
	case e.ThreadList != nil && e.ThreadList.Updated != nil:
		w.threads[e.ThreadList.Updated.ID] = true
		w.dirty[e.ThreadList.Updated.ID] = true
	case e.ThreadList != nil && e.ThreadList.RemovedID != "":
		delete(w.threads, e.ThreadList.RemovedID)
		delete(w.dirty, e.ThreadList.RemovedID)
		return
	case e.GH != nil:
		if _, ok := e.GH["viewerUpdated"]; ok && len(e.GH) == 1 {
			return
		}
		w.all = true // pull requests or checks may have changed
	default:
		return
	}
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

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
		w.mu.Lock()
		var ids []string
		if w.all {
			for id := range w.threads {
				ids = append(ids, id)
			}
		} else {
			for id := range w.dirty {
				if w.threads[id] {
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
			w.setFailing(thread, false)
			if _, ok := w.state.Flagged[thread]; ok {
				w.clear(ctx, thread)
			}
			return
		}
		w.h.Log().Debug("reading checks", "thread", thread, "err", err)
		return
	}
	st := summarize(pc)
	if fp := fingerprint(pc); w.prints[thread] != fp {
		if w.prints[thread] != "" {
			w.a.poke(pc.Repo, pc.Number)
		}
		w.prints[thread] = fp
	}
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
			w.h.Log().Info("flagged", "thread", thread, "job", st.Job, "run", st.RunID)
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

func (w *watcher) clear(ctx context.Context, thread string) {
	if err := w.h.SetFlag(ctx, pluginsdk.Flag{ThreadID: thread, Level: pluginsdk.FlagNone}); err != nil &&
		pluginsdk.ErrorCode(err) != pluginsdk.CodeNotFound && pluginsdk.ErrorCode(err) != pluginsdk.CodeUnavailable {
		w.h.Log().Warn("clearing the flag", "thread", thread, "err", err)
		return
	}
	w.h.Log().Info("cleared", "thread", thread)
	delete(w.state.Flagged, thread)
	w.save()
}

func (w *watcher) retry(thread string) {
	w.mu.Lock()
	w.dirty[thread] = true
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
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
