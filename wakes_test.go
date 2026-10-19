package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/plugintest"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

const (
	prArg    = "alexwaumann/code-foundry#197"
	runsCall = "api repos/" + repo + "/actions/runs?head_sha=" + headSHA + "&per_page=50"
	wantHint = "code-foundry thread subscribe --plugin gh-actions/run-finished --arg pr=alexwaumann/code-foundry#197"
)

// manifestWakes are plugin.json's wake sources, for plugintest.Options.Wakes (the fake
// then refuses a fire the host would).
func manifestWakes(t *testing.T) []pluginsdk.WakeSpec {
	t.Helper()
	data, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := pluginsdk.LoadManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return m.Wakes
}

func TestManifestWakes(t *testing.T) {
	ws := manifestWakes(t)
	if len(ws) != 1 || ws[0].Name != wakeRunFinished || ws[0].ScopeOr() != pluginsdk.WakeScopePR {
		t.Fatalf("wakes = %+v", ws)
	}
	pr, ok := ws[0].Arg("pr")
	if !ok || pr.Context != pluginsdk.WakeContextPR || !pr.Required {
		t.Errorf("pr arg = %+v", pr)
	}
	if run, ok := ws[0].Arg("run"); !ok || run.Required || run.TypeOr() != pluginsdk.ArgInt {
		t.Errorf("run arg = %+v", run)
	}
}

// runsAt is the fixture's failed run (ci #494) with another status, conclusion and attempt.
func runsAt(t *testing.T, status, conclusion string, attempt int) []byte {
	t.Helper()
	var res struct {
		WorkflowRuns []map[string]any `json:"workflow_runs"`
	}
	if err := json.Unmarshal(fixture(t, "runs_failed.json"), &res); err != nil {
		t.Fatal(err)
	}
	r := res.WorkflowRuns[0]
	r["status"], r["run_attempt"] = status, attempt
	if conclusion == "" {
		r["conclusion"] = nil
	} else {
		r["conclusion"] = conclusion
	}
	return mustJSON(res)
}

func (f *fakeGH) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func startWakes(t *testing.T) *harness {
	t.Helper()
	hs := start(t, plugintest.Options{Wakes: manifestWakes(t)}, func(a *app) { a.wakePoll, a.wakeIdlePoll = 10*time.Millisecond, 10*time.Millisecond })
	realFailure(t, hs.gh)
	return hs
}

// waitCalls waits until gh has been asked for the runs n more times than before.
func waitCalls(t *testing.T, hs *harness, n int) {
	t.Helper()
	from := hs.gh.count(runsCall)
	hs.WaitFor(5*time.Second, func(*plugintest.Host) bool { return hs.gh.count(runsCall) >= from+n })
}

// Subscribe, a run finishes, the wake is fired with the pull request and run as the
// match; a re-arm does not double it; unsubscribing stops the polling.
func TestRunFinishedWake(t *testing.T) {
	hs := startWakes(t)
	hs.gh.set(runsCall, runsAt(t, "in_progress", "", 1), nil)
	uctx := pluginsdk.UiContext{ThreadID: "s-1"}
	sub, err := hs.SubscribeWake(wakeRunFinished, map[string]string{"pr": prArg}, uctx)
	if err != nil {
		t.Fatal(err)
	}
	// One that waits for another run only: the fire does not reach it.
	other, err := hs.SubscribeWake(wakeRunFinished, map[string]string{"pr": prArg, "run": "1"}, uctx)
	if err != nil {
		t.Fatal(err)
	}
	waitCalls(t, hs, 2) // the baseline is in
	if len(hs.WakeFires()) != 0 {
		t.Fatalf("fired for a run in progress: %+v", hs.WakeFires())
	}

	hs.gh.set(runsCall, runsAt(t, "completed", "failure", 1), nil)
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 1 })
	f := hs.WakeFires()[0]
	if f.Source != wakeRunFinished || f.Match["pr"] != prArg || f.Match["run"] != fmt.Sprint(failRun) || f.Title != "ci #494 failed" {
		t.Errorf("fire = %+v", f)
	}
	if d := hs.WakeDelivered(); d[0] != 1 {
		t.Errorf("delivered %v, want the pull request's subscription only", d)
	}
	var p struct {
		Run        int64  `json:"run"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		URL        string `json:"url"`
		FailedJobs []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Step string `json:"step"`
		} `json:"failed_jobs"`
	}
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Run != failRun || p.Name != "ci" || p.Status != "completed" || p.Conclusion != "failure" ||
		!strings.Contains(p.URL, fmt.Sprint(failRun)) || len(p.FailedJobs) != 1 || p.FailedJobs[0].Name != "go-test" || p.FailedJobs[0].ID != failJob {
		t.Errorf("payload = %s", f.Payload)
	}

	// The same run seen again fires nothing; a restart's re-arm keeps one poller.
	waitCalls(t, hs, 3)
	if _, err := hs.SubscribeWakeWith(pluginsdk.WakeSubscription{Subscription: sub, Source: wakeRunFinished, Args: map[string]string{"pr": prArg}, Context: uctx, Repeat: true}); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, hs, 3)
	if n := len(hs.WakeFires()); n != 1 {
		t.Errorf("%d fires for one finished run", n)
	}

	// A rerun: attempt 2 runs, then passes, and fires again.
	hs.gh.set(runsCall, runsAt(t, "in_progress", "", 2), nil)
	waitCalls(t, hs, 2)
	hs.gh.set(runsCall, runsAt(t, "completed", "success", 2), nil)
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 2 })
	if f := hs.WakeFires()[1]; f.Title != "ci #494 passed (attempt 2)" {
		t.Errorf("rerun fire = %+v", f)
	}

	// Unsubscribed from both: the poller stops.
	for _, id := range []string{sub, other} {
		if err := hs.UnsubscribeWake(id); err != nil {
			t.Fatal(err)
		}
	}
	hs.WaitFor(5*time.Second, func(*plugintest.Host) bool { return !hs.app.wakes.watching(prArg) })
	n := hs.gh.count(runsCall)
	time.Sleep(100 * time.Millisecond)
	if m := hs.gh.count(runsCall); m > n+1 {
		t.Errorf("still polling after the last unsubscription: %d more calls", m-n)
	}
}

// A one-shot subscription goes after its fire (the host sends wake/unsubscribed), and
// the poller with it; runs that had finished before it was armed fire nothing.
func TestRunFinishedOneShot(t *testing.T) {
	hs := startWakes(t)
	hs.gh.set(runsCall, runsAt(t, "completed", "failure", 1), nil)
	if _, err := hs.SubscribeWakeWith(pluginsdk.WakeSubscription{Source: wakeRunFinished, Args: map[string]string{"pr": prArg}, Context: pluginsdk.UiContext{ThreadID: "s-1"}}); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, hs, 3)
	if len(hs.WakeFires()) != 0 {
		t.Fatalf("fired for a run that finished before the subscription: %+v", hs.WakeFires())
	}
	hs.gh.set(runsCall, runsAt(t, "completed", "success", 2), nil)
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 1 })
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.WakeSubscriptions()) == 0 && !hs.app.wakes.watching(prArg) })
}

// An open panel's fetch counts: the wake fires from what the panel saw.
func TestRunFinishedFromPanel(t *testing.T) {
	hs := start(t, plugintest.Options{Wakes: manifestWakes(t)}, func(a *app) { a.wakePoll, a.wakeIdlePoll = time.Hour, time.Hour })
	realFailure(t, hs.gh)
	hs.gh.set(runsCall, runsAt(t, "in_progress", "", 1), nil)
	if _, err := hs.SubscribeWake(wakeRunFinished, map[string]string{"pr": prArg}, pluginsdk.UiContext{ThreadID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, hs, 1) // the poller's baseline, then it sleeps an hour
	hs.gh.set(runsCall, runsAt(t, "completed", "failure", 1), nil)
	openRuns(t, hs)
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.WakeFires()) == 1 })
}

// Every message about a run ends with the line that waits for it: the excerpt sent to
// Claude, rerun and cancel results, the panel's notice. The flag reason does not (80
// characters).
func TestWakeHints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := startWakes(t)
	hs.gh.set(fmt.Sprintf("run rerun %d -R %s --failed", failRun, repo), nil, nil)
	uctx := pluginsdk.UiContext{ThreadID: "s-1"}

	if _, err := hs.Command(ctx, "send-failure", map[string]string{"thread": "s-1"}, uctx); err != nil {
		t.Fatal(err)
	}
	sent := lastSend(hs)
	if sent == nil || !strings.HasSuffix(sent.Args["text"], "\n"+wantHint) {
		t.Errorf("send-failure text does not end with the hint:\n%v", sent)
	}

	res, err := hs.Command(ctx, "rerun-failed", map[string]string{"thread": "s-1"}, uctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%s --arg run=%d", wantHint, failRun); !strings.HasSuffix(res.Message, want) {
		t.Errorf("rerun-failed message %q, want it to end with %q", res.Message, want)
	}

	openRuns(t, hs)
	v := hs.waitView(t, "the wait notice", func(v ui.View) bool { return node(v, "wait") != nil })
	n := node(v, "wait").Notice
	if n.Title != "ci #494 failed" || n.Detail != wantHint {
		t.Errorf("notice = %+v", n)
	}

	_ = hs.Event(threadsSnapshot("s-1"))
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(flagsFor(h, "s-1")) == 1 })
	if r := flagsFor(hs.Host, "s-1")[0].Reason; len(r) > 80 || strings.Contains(r, "code-foundry") {
		t.Errorf("flag reason %q", r)
	}
}

func TestParsePRArg(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"alexwaumann/code-foundry#197", "alexwaumann/code-foundry#197", true},
		{"Octo/App#12", "octo/app#12", true},
		{"octo/app#0", "", false},
		{"octo#12", "", false},
		{"octo/app/x#12", "", false},
		{"#12", "", false},
		{"octo/app", "", false},
		{"", "", false},
	} {
		pr, ok := parsePRArg(tt.in)
		if ok != tt.ok || ok && prKey(pr.Repo, pr.Number) != tt.want {
			t.Errorf("parsePRArg(%q) = %+v %v", tt.in, pr, ok)
		}
	}
}

func TestRunTitle(t *testing.T) {
	for _, tt := range []struct {
		r    ghRun
		want string
	}{
		{ghRun{Name: "ci", RunNumber: 507, Conclusion: "success", RunAttempt: 1}, "ci #507 passed"},
		{ghRun{Name: "ci", RunNumber: 494, Conclusion: "failure", RunAttempt: 2}, "ci #494 failed (attempt 2)"},
		{ghRun{Name: "release", RunNumber: 3, Conclusion: "timed_out"}, "release #3 timed out"},
		{ghRun{ID: 9, RunNumber: 1, Conclusion: "cancelled"}, "run 9 #1 cancelled"},
	} {
		if got := runTitle(tt.r); got != tt.want {
			t.Errorf("runTitle(%+v) = %q, want %q", tt.r, got, tt.want)
		}
	}
}
