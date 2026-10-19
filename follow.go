package main

import (
	"context"
	"time"
)

// follow re-reads a thread's checks once a run this plugin reran or cancelled has
// finished. pullRequestChanged cannot be relied on for that: a rerun that ends the way
// the run did (failed again, or cancelled and rerun to the same result) leaves the
// check rollup where it was, and when it starts and ends between two of Code Foundry's
// GitHub polls no event says it ran (seen at the 60 s tier with a 27 s attempt; at the
// 15 minute tier almost every rerun is like that). So the plugin watches the one run
// it touched, through gh every wakePoll, until it completes or followMax passes.
// rerun: wait for the run to restart before taking "completed" as the end.
func (w *watcher) follow(t target, rerun bool) {
	if t.Run.ID == 0 || w.ctx == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(w.ctx, w.a.followMax)
		defer cancel()
		going := !rerun
		for {
			r, err := w.a.gh.run(ctx, t.PR.Repo, t.Run.ID)
			switch {
			case err != nil:
				if ctx.Err() == nil {
					w.h.Log().Debug("following a run", "run", t.Run.ID, "err", err)
				}
			case !completed(r.Status):
				going = true
			case going || t.Run.RunAttempt > 0 && r.RunAttempt > t.Run.RunAttempt:
				w.h.Log().Info("followed run finished", "thread", t.Thread, "run", t.Run.ID, "attempt", r.RunAttempt, "conclusion", r.Conclusion)
				w.mu.Lock()
				w.notBefore[t.Thread] = r.UpdatedAt
				w.mu.Unlock()
				w.retry(t.Thread)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.a.wakePoll):
			}
		}
	}()
}

// fresh says whether a thread's checks answer is at least as new as the end of a run
// it followed; when not (the host answered from a copy fetched before the run ended,
// which it keeps for up to 30 s), it reads again after followRecheck, a few times.
// Worker goroutine only.
func (w *watcher) fresh(thread string, pc prChecks) bool {
	w.mu.Lock()
	nb, ok := w.notBefore[thread]
	w.mu.Unlock()
	if !ok {
		return true
	}
	if !pc.FetchedAt.Before(nb) || w.stale[thread] >= 6 {
		w.mu.Lock()
		delete(w.notBefore, thread)
		w.mu.Unlock()
		delete(w.stale, thread)
		return true
	}
	w.stale[thread]++
	time.AfterFunc(w.a.followRecheck, func() {
		if w.ctx.Err() == nil {
			w.retry(thread)
		}
	})
	return false
}
