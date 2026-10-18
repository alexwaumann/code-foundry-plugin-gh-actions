// Command gh-actions is a Code Foundry plugin that tracks the GitHub Actions runs of a
// thread's pull request: a side-panel surface with the head commit's runs, the selected
// run's jobs and its failed step's log; commands to rerun, cancel and open a run; and an
// attention flag on the thread when a run fails. It talks to GitHub through the user's
// own `gh` login.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

func main() {
	if err := pluginsdk.Serve(plugin()); err != nil {
		fmt.Fprintln(os.Stderr, "gh-actions:", err)
		os.Exit(1)
	}
}

// plugin serves everything plugin.json declares, with the real gh.
func plugin() pluginsdk.Plugin { return newApp(execGH{}).plugin() }

// app is the plugin's state across commands, surfaces and events.
type app struct {
	gh  gh
	now func() time.Time

	// Poll intervals of an open surface: while a run is in progress and someone looks,
	// while nobody looks, and while the commit has no runs yet.
	poll, hiddenPoll, waitPoll, rateLimitPoll time.Duration
	// debounce batches the flag checks a burst of events asks for; sweep re-checks the
	// threads with Actions checks in progress, sweepAll every thread.
	debounce, sweep, sweepAll time.Duration

	mu       sync.Mutex
	surfaces map[string]*runsSurface
	watch    *watcher
}

func newApp(r ghRunner) *app {
	return &app{
		gh:            gh{r: r},
		now:           time.Now,
		poll:          15 * time.Second,
		hiddenPoll:    60 * time.Second,
		waitPoll:      30 * time.Second,
		rateLimitPoll: 5 * time.Minute,
		debounce:      2 * time.Second,
		sweep:         time.Minute,
		sweepAll:      5 * time.Minute,
		surfaces:      map[string]*runsSurface{},
	}
}

func (a *app) plugin() pluginsdk.Plugin {
	return pluginsdk.Plugin{
		Init: func(ctx context.Context, h *pluginsdk.Host) error {
			h.Log().Info("gh-actions is up", "host", h.HostVersion(), "dev", h.Dev())
			a.watch = newWatcher(a, h)
			return a.watch.start(ctx)
		},
		OnEvent: func(_ context.Context, _ *pluginsdk.Host, ev pluginsdk.Event) {
			if a.watch != nil {
				a.watch.onEvent(ev)
			}
		},
		Shutdown: func(context.Context, *pluginsdk.Host) {
			if a.watch != nil {
				a.watch.stop()
			}
		},
		Commands: map[string]pluginsdk.CommandFunc{
			"rerun-failed": a.rerunFailed,
			"rerun":        a.rerunAll,
			"cancel":       a.cancel,
			"open":         a.open,
			"send-failure": a.sendFailure,
		},
		Surfaces: map[string]pluginsdk.SurfaceFactory{"runs": a.newRuns},
	}
}

// poke makes the open surfaces of a pull request fetch again (after a rerun or a
// cancel, or when its head moved).
func (a *app) poke(repo string, number int) {
	a.mu.Lock()
	var hit []*runsSurface
	for _, s := range a.surfaces {
		if s.isFor(repo, number) {
			hit = append(hit, s)
		}
	}
	a.mu.Unlock()
	for _, s := range hit {
		s.wakeUp()
	}
}
