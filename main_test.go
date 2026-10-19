package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexwaumann/code-foundry/pluginsdk"
	"github.com/alexwaumann/code-foundry/pluginsdk/plugintest"
	"github.com/alexwaumann/code-foundry/pluginsdk/ui"
)

const (
	repo    = "alexwaumann/code-foundry"
	headSHA = "a6b1f0c2d3e4f5061728394a5b6c7d8e9f001122"
	failRun = int64(39125565731)
	failJob = int64(117669858813)
)

// plugin.json is valid and declares exactly what the plugin serves.
func TestManifest(t *testing.T) {
	data, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	m, warnings, err := pluginsdk.LoadManifest(data)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("plugin.json: %v %v", err, warnings)
	}
	p := plugin()
	for _, c := range m.Commands {
		if p.Commands[c.Name] == nil {
			t.Errorf("command %s is declared but not served", c.Name)
		}
	}
	for _, s := range m.Surfaces {
		if p.Surfaces[s.ID] == nil {
			t.Errorf("surface %s is declared but not served", s.ID)
		}
	}
	if len(p.Commands) != len(m.Commands) || len(p.Surfaces) != len(m.Surfaces) {
		t.Errorf("serves %d commands and %d surfaces, declares %d and %d", len(p.Commands), len(p.Surfaces), len(m.Commands), len(m.Surfaces))
	}
	// Every host command the views and code use is granted.
	for _, c := range []string{"thread.send", "view.open.url", "pr.checks"} {
		if !m.Permissions.AllowsCommand(c) {
			t.Errorf("permissions.commands lacks %s", c)
		}
	}
}

// fakeGH answers gh calls from a table keyed by the joined args.
type fakeGH struct {
	mu    sync.Mutex
	resp  map[string]fakeResp
	calls []string
}

type fakeResp struct {
	out []byte
	err error
}

func newFakeGH() *fakeGH { return &fakeGH{resp: map[string]fakeResp{}} }

func (f *fakeGH) Run(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	r, ok := f.resp[key]
	if !ok {
		return nil, classify(errors.New("exit status 1"), "gh: Not Found (HTTP 404)")
	}
	return r.out, r.err
}

func (f *fakeGH) set(key string, out []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp[key] = fakeResp{out, err}
}

func (f *fakeGH) called(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == key {
			return true
		}
	}
	return false
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// realFailure loads a real failed run (ci #… on code-foundry's main, go-test failing)
// as the head of pull request #197.
func realFailure(t *testing.T, f *fakeGH) {
	f.set("api repos/"+repo+"/pulls/197", mustJSON(map[string]any{
		"title": "plugins 4: the code-foundry-plugin skill", "html_url": "https://github.com/" + repo + "/pull/197",
		"state": "open", "head": map[string]any{"sha": headSHA, "ref": "cf/plugins-4-skill"}}), nil)
	f.set("api repos/"+repo+"/actions/runs?head_sha="+headSHA+"&per_page=50", fixture(t, "runs_failed.json"), nil)
	f.set(fmt.Sprintf("api repos/%s/actions/runs/%d/jobs?per_page=100", repo, failRun), fixture(t, "jobs_failed.json"), nil)
	f.set(fmt.Sprintf("run view --job %d --log-failed -R %s", failJob, repo), fixture(t, "log_failed.txt"), nil)
}

// checksJSON is what pr.checks answers for #197 with Actions checks of conclusion c
// for the jobs given (run/job ids from the fixture).
func checksJSON(conclusions map[string]string) []byte {
	ids := map[string]int64{"attested": 117669843514, "go-test": failJob, "frontend": 117669858838, "go-lint": 117669858971}
	var checks []map[string]any
	for _, name := range []string{"attested", "go-test", "frontend", "go-lint"} {
		c, ok := conclusions[name]
		if !ok {
			c = "success"
		}
		status := "completed"
		if c == "" {
			status = "in_progress"
		}
		checks = append(checks, map[string]any{"name": name, "workflow": "ci", "status": status, "conclusion": c,
			"app": "github-actions", "provider": "github-actions",
			"url": fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", repo, failRun, ids[name])})
	}
	checks = append(checks, map[string]any{"name": "local-check", "status": "completed", "conclusion": "success", "provider": "status"})
	return mustJSON(map[string]any{"repo": repo, "number": 197, "url": "https://github.com/" + repo + "/pull/197",
		"headSha": headSHA, "checks": checks})
}

// harness starts the plugin with a fake gh and a pr.checks that answers checks().
type harness struct {
	*plugintest.Host
	gh        *fakeGH
	app       *app
	mu        sync.Mutex
	checks    []byte // nil: the thread has no pull request
	checksErr error  // set: pr.checks fails with it
}

func start(t *testing.T, opts plugintest.Options, tweaks ...func(*app)) *harness {
	t.Helper()
	hs := &harness{gh: newFakeGH(), checks: checksJSON(map[string]string{"go-test": "failure"})}
	hs.app = newApp(hs.gh)
	hs.app.poll, hs.app.hiddenPoll, hs.app.waitPoll, hs.app.debounce = 20*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	hs.app.retryMin, hs.app.retryMax = time.Hour, time.Hour
	for _, tw := range tweaks {
		tw(hs.app)
	}
	opts.Name = "gh-actions"
	if opts.Granted.Commands == nil {
		opts.Granted = pluginsdk.Permissions{Commands: []string{"thread.send", "view.open.url", "pr.checks"},
			Events: []string{"gh", "threads"}, ThreadStatus: true}
	}
	opts.Invoke = func(_ context.Context, p pluginsdk.InvokeParams) (pluginsdk.CommandResult, error) {
		switch p.Command {
		case "pr.checks":
			hs.mu.Lock()
			defer hs.mu.Unlock()
			if hs.checksErr != nil {
				return pluginsdk.CommandResult{}, hs.checksErr
			}
			if hs.checks == nil {
				return pluginsdk.CommandResult{}, pluginsdk.NewError(pluginsdk.CodeUnavailable, "not available in this context: thread %s has no pull request", p.Args["thread"])
			}
			return pluginsdk.CommandResult{JSON: hs.checks}, nil
		case "thread.send":
			return pluginsdk.CommandResult{Message: "sent"}, nil
		case "view.open.url":
			return pluginsdk.CommandResult{Message: "opened"}, nil
		}
		return pluginsdk.CommandResult{}, pluginsdk.NewError(pluginsdk.CodeNotFound, "unknown command %s", p.Command)
	}
	hs.Host = plugintest.Start(t, hs.app.plugin(), opts)
	// Stop the plugin (and so the flag watcher, which writes flags.json) before the
	// data dir is removed.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	})
	return hs
}

func (hs *harness) setChecks(b []byte) {
	hs.mu.Lock()
	hs.checks = b
	hs.mu.Unlock()
}

const inst = "gh-actions/runs/pr:alexwaumann/code-foundry#197"

// view decodes the instance's last view and checks it as the host will.
func (hs *harness) view(t *testing.T) ui.View {
	t.Helper()
	raw, ok := hs.View(inst)
	if !ok {
		t.Fatal("no view")
	}
	var v ui.View
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("invalid view: %v\n%s", err, raw)
	}
	return v
}

// waitView waits until the last view satisfies cond.
func (hs *harness) waitView(t *testing.T, what string, cond func(ui.View) bool) ui.View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, ok := hs.View(inst)
		if ok {
			var v ui.View
			if json.Unmarshal(raw, &v) == nil && cond(v) {
				if err := v.Validate(); err != nil {
					t.Fatalf("invalid view: %v\n%s", err, raw)
				}
				return v
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last view:\n%s", what, raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func node(v ui.View, key string) *ui.Node {
	var find func(ns []ui.Node) *ui.Node
	find = func(ns []ui.Node) *ui.Node {
		for i := range ns {
			if ns[i].Key == key {
				return &ns[i]
			}
			if ns[i].Section != nil {
				if n := find(ns[i].Section.Children); n != nil {
					return n
				}
			}
		}
		return nil
	}
	return find(v.Nodes)
}

// logNode is the view's log node (its key changes with the run and selection).
func logNode(v ui.View) *ui.Node {
	var find func(ns []ui.Node) *ui.Node
	find = func(ns []ui.Node) *ui.Node {
		for i := range ns {
			if ns[i].Log != nil {
				return &ns[i]
			}
			if ns[i].Section != nil {
				if n := find(ns[i].Section.Children); n != nil {
					return n
				}
			}
		}
		return nil
	}
	return find(v.Nodes)
}

func buttons(v ui.View) map[string]ui.Button {
	out := map[string]ui.Button{}
	if n := node(v, "bar"); n != nil {
		for _, b := range n.Actions.Buttons {
			out[b.Key] = b
		}
	}
	return out
}

func act(t *testing.T, hs *harness, a pluginsdk.SurfaceAct) {
	t.Helper()
	a.Instance = inst
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Act(ctx, a); err != nil {
		t.Fatalf("act %s %s: %v", a.Action, a.Key, err)
	}
}

func openRuns(t *testing.T, hs *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.OpenSurface(ctx, inst, "runs", pluginsdk.UiContext{ThreadID: "s-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunsSurfaceFailedRun(t *testing.T) {
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	openRuns(t, hs)

	// Runs: the failed run, selected, with the header naming it.
	v := hs.waitView(t, "the jobs", func(v ui.View) bool { return v.Tabs != nil && len(buttons(v)) > 0 && buttons(v)["btn-send"].Key != "" })
	if v.Header.Title != "#197 plugins 4: the code-foundry-plugin skill" {
		t.Errorf("title %q", v.Header.Title)
	}
	if v.Header.Pill == nil || v.Header.Pill.Label != "failed" || v.Header.Pill.Tone != ui.ToneError {
		t.Errorf("pill %+v", v.Header.Pill)
	}
	if v.Tabs.Active != tabRuns {
		t.Errorf("active tab %s", v.Tabs.Active)
	}
	runs := node(v, "run-list")
	if runs == nil || len(runs.List.Rows) != 1 {
		t.Fatalf("runs node %+v", runs)
	}
	row := runs.List.Rows[0]
	if row.Title != "ci" || row.Glyph.Status != ui.StatusFailed || !row.Selected || row.Trailing == nil || row.Trailing.Spans[0].Duration == nil {
		t.Errorf("run row %+v", row)
	}
	b := buttons(v)
	for key, cmd := range map[string]string{"btn-rerun-failed": "plugin.gh-actions.rerun-failed", "btn-rerun": "plugin.gh-actions.rerun"} {
		if b[key].Action == nil || b[key].Action.Command == nil || b[key].Action.Command.Name != cmd || b[key].Action.Command.Args["run"] != fmt.Sprint(failRun) {
			t.Errorf("button %s = %+v", key, b[key])
		}
	}
	if _, ok := b["btn-cancel"]; ok {
		t.Error("a finished run offers Cancel")
	}
	if len(v.Header.Actions) != 2 || v.Header.Actions[0].Action.OpenURL != "https://github.com/"+repo+"/actions/runs/39125565731" {
		t.Errorf("header actions %+v", v.Header.Actions)
	}

	// Jobs: clicking the run shows its jobs, the failed one naming its step.
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActClick, Action: "select-run", Data: fmt.Sprint(failRun)})
	v = hs.view(t)
	if v.Tabs.Active != tabJobs {
		t.Fatalf("active tab %s", v.Tabs.Active)
	}
	jobs := node(v, "job-list")
	if jobs == nil || len(jobs.List.Rows) != 4 {
		t.Fatalf("jobs %+v", jobs)
	}
	var goTest ui.Row
	for _, r := range jobs.List.Rows {
		if r.Title == "go-test" {
			goTest = r
		}
	}
	if goTest.Glyph == nil || goTest.Glyph.Status != ui.StatusFailed || goTest.Line2 == nil || !strings.Contains(fmt.Sprint(goTest.Line2.Spans), "Run make go-test") {
		t.Errorf("go-test row %+v", goTest)
	}
	if v.Tabs.Items[1].Badge == nil || v.Tabs.Items[1].Badge.Label != "1 failed" {
		t.Errorf("jobs tab %+v", v.Tabs.Items[1])
	}

	// Log: the failed step's tail, toned, without timestamps or colour codes.
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActTab, Action: "tab", Key: tabLog, Value: tabLog})
	v = hs.view(t)
	log := logNode(v)
	if log == nil || log.Log.Title != "go-test · Run make go-test" || log.Key != fmt.Sprintf("log-%d-1-all", failRun) {
		t.Fatalf("log node %+v", log)
	}
	var failLine, errLine bool
	for _, l := range log.Log.Lines {
		if strings.Contains(l.Text, "\x1b") || strings.Contains(l.Text, "^[") || strings.HasPrefix(l.Text, "2026-") {
			t.Errorf("unclean line %q", l.Text)
		}
		if strings.HasPrefix(l.Text, "--- FAIL: TestCLINotifyReachesWatcher") && l.Tone == ui.ToneError {
			failLine = true
		}
		if l.Text == "Error: Process completed with exit code 2." && l.Tone == ui.ToneError {
			errLine = true
		}
	}
	if !failLine || !errLine {
		t.Errorf("log lines lack the toned failures: %+v", log.Log.Lines)
	}

	// Back and forth: leaving the tab drops the buffer; coming back re-sends it.
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActTab, Action: "tab", Key: tabRuns, Value: tabRuns})
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActTab, Action: "tab", Key: tabLog, Value: tabLog})
	if n := logNode(hs.view(t)); n == nil || len(n.Log.Lines) == 0 {
		t.Error("returning to the Log tab sent no lines")
	}

	// Send failure to Claude is a command button: the host runs it with the viewer's
	// panel context, so the thread is the one looking (s-2), not the opener (s-1).
	send := buttons(hs.view(t))["btn-send"]
	if send.Action == nil || send.Action.Command == nil || send.Action.Command.Name != "plugin.gh-actions.send-failure" ||
		send.Action.Command.Args["run"] != fmt.Sprint(failRun) || send.Action.Command.Args["job"] != fmt.Sprint(failJob) {
		t.Fatalf("send button %+v", send)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := hs.Command(ctx, "send-failure", send.Action.Command.Args, pluginsdk.UiContext{ThreadID: "s-2"})
	if err != nil || res.Message != "Sent go-test's failure to Claude" {
		t.Fatalf("send-failure = %q, %v", res.Message, err)
	}
	sent := lastSend(hs)
	if sent == nil {
		t.Fatal("no thread.send")
	}
	text := sent.Args["text"]
	if sent.Context.ThreadID != "s-2" || len(text) > 4096 {
		t.Errorf("send to %s, %d bytes", sent.Context.ThreadID, len(text))
	}
	for _, want := range []string{"Workflow: ci", "Job: go-test", "Step: Run make go-test", "/actions/runs/39125565731/job/117669858813", "--- FAIL: TestCLISettings", "daemon did not become ready", "Error: Process completed with exit code 2.", "investigate"} {
		if !strings.Contains(text, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, text)
		}
	}

	// The event an older view's button sends: the acting panel's thread (act.context),
	// else the opener's.
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActClick, Action: "send-failure", Key: "btn-send", Context: pluginsdk.UiContext{ThreadID: "s-3"}})
	if s := lastSend(hs); s == nil || s.Context.ThreadID != "s-3" {
		t.Errorf("act with context sent to %+v", s)
	}
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActClick, Action: "send-failure", Key: "btn-send"})
	if s := lastSend(hs); s == nil || s.Context.ThreadID != "s-1" {
		t.Errorf("act without context sent to %+v", s)
	}

	if err := hs.Act(context.Background(), pluginsdk.SurfaceAct{Instance: inst, Kind: pluginsdk.ActClick, Action: "select-run", Data: "1"}); err == nil {
		t.Error("an unknown run was accepted")
	}
	if err := hs.Act(context.Background(), pluginsdk.SurfaceAct{Instance: inst, Kind: pluginsdk.ActClick, Action: "nope"}); err == nil {
		t.Error("an unknown action was accepted")
	}
}

func lastSend(hs *harness) *pluginsdk.InvokeParams {
	var sent *pluginsdk.InvokeParams
	for _, inv := range hs.Invocations() {
		if inv.Command == "thread.send" {
			sent = &inv
		}
	}
	return sent
}

func TestSendFailureWithoutPermission(t *testing.T) {
	hs := start(t, plugintest.Options{Granted: pluginsdk.Permissions{Commands: []string{"pr.checks"}}})
	realFailure(t, hs.gh)
	openRuns(t, hs)
	hs.waitView(t, "the send button", func(v ui.View) bool { return buttons(v)["btn-send"].Key != "" })
	_, err := hs.Command(context.Background(), "send-failure", nil, pluginsdk.UiContext{ThreadID: "s-1"})
	if pluginsdk.ErrorCode(err) != pluginsdk.CodePermissionDenied || !strings.Contains(err.Error(), "sending the failure") {
		t.Errorf("err = %v (code %d)", err, pluginsdk.ErrorCode(err))
	}
}

// send-failure without an open surface reads the run, its jobs and the failed log from
// GitHub; --job names a job that must have failed.
func TestSendFailureCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	var runs struct {
		WorkflowRuns []json.RawMessage `json:"workflow_runs"`
	}
	if err := json.Unmarshal(fixture(t, "runs_failed.json"), &runs); err != nil {
		t.Fatal(err)
	}
	hs.gh.set(fmt.Sprintf("api repos/%s/actions/runs/%d", repo, failRun), runs.WorkflowRuns[0], nil)
	for _, tt := range []struct {
		args map[string]string
		want string
		code int
	}{
		{map[string]string{"thread": "s-7"}, "Sent go-test's failure to Claude", 0},
		{map[string]string{"thread": "s-7", "run": fmt.Sprint(failRun)}, "Sent go-test's failure to Claude", 0},
		{map[string]string{"thread": "s-7", "run": fmt.Sprint(failRun), "job": fmt.Sprint(failJob)}, "Sent go-test's failure to Claude", 0},
		{map[string]string{"thread": "s-7", "run": fmt.Sprint(failRun), "job": "117669858838"}, "job frontend of ci #494 did not fail", pluginsdk.CodeUnavailable},
		{map[string]string{"thread": "s-7", "job": "nope"}, `job "nope" is not a job id`, pluginsdk.CodeInvalidParams},
	} {
		res, err := hs.Command(ctx, "send-failure", tt.args, pluginsdk.UiContext{})
		got := res.Message
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, tt.want) || pluginsdk.ErrorCode(err) != tt.code {
			t.Errorf("send-failure %v = %q (code %d), want %q (code %d)", tt.args, got, pluginsdk.ErrorCode(err), tt.want, tt.code)
			continue
		}
		if err == nil {
			if s := lastSend(hs); s == nil || s.Context.ThreadID != "s-7" || !strings.Contains(s.Args["text"], "Workflow: ci (run #494") {
				t.Errorf("sent %+v", s)
			}
		}
	}
}

// Runs GitHub created in the same second: the newest is the higher id. The real pair
// is ci #506 (cancelled by #507's concurrency group) and #507 (passed), listed by the
// API with #506 first.
func TestPickRun(t *testing.T) {
	var real struct {
		WorkflowRuns []ghRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(fixture(t, "runs_same_second.json"), &real); err != nil {
		t.Fatal(err)
	}
	if len(real.WorkflowRuns) != 2 || real.WorkflowRuns[0].RunNumber != 506 || !real.WorkflowRuns[0].CreatedAt.Equal(real.WorkflowRuns[1].CreatedAt) {
		t.Fatalf("fixture: %+v", real.WorkflowRuns)
	}
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	run := func(id int64, created, status, conclusion string) ghRun {
		return ghRun{ID: id, CreatedAt: at(created), Status: status, Conclusion: conclusion}
	}
	const t0, t1 = "2026-10-18T14:50:04Z", "2026-10-18T14:52:00Z"
	for _, tt := range []struct {
		name  string
		runs  []ghRun
		order []int64
		want  int64
	}{
		{"real #506 cancelled, #507 passed, same second", real.WorkflowRuns, []int64{39137760005, 39137758970}, 39137760005},
		{"same second, the newer failed", []ghRun{run(1, t0, "completed", "success"), run(2, t0, "completed", "failure")}, []int64{2, 1}, 2},
		{"same second, the older failed", []ghRun{run(2, t0, "completed", "success"), run(1, t0, "completed", "failure")}, []int64{2, 1}, 1},
		{"newest passed, an older one still running", []ghRun{run(1, t0, "in_progress", ""), run(2, t1, "completed", "success")}, []int64{2, 1}, 1},
		{"newest queued before an older failure", []ghRun{run(1, t0, "completed", "failure"), run(2, t1, "queued", "")}, []int64{2, 1}, 2},
		{"all passed or cancelled: the newest", []ghRun{run(1, t0, "completed", "cancelled"), run(3, t1, "completed", "success"), run(2, t1, "completed", "cancelled")}, []int64{3, 2, 1}, 3},
		{"timed out counts as failed", []ghRun{run(1, t0, "completed", "timed_out"), run(2, t1, "completed", "success")}, []int64{2, 1}, 1},
		{"no runs", nil, nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runs := append([]ghRun(nil), tt.runs...)
			sortRuns(runs)
			var order []int64
			for _, r := range runs {
				order = append(order, r.ID)
			}
			if fmt.Sprint(order) != fmt.Sprint(tt.order) {
				t.Errorf("order %v, want %v", order, tt.order)
			}
			if got := pickRun(runs); got != tt.want {
				t.Errorf("pickRun = %d, want %d", got, tt.want)
			}
		})
	}
}

// The gh call lists runs newest first whatever order the API answers in.
func TestRunsSortedFromAPI(t *testing.T) {
	f := newFakeGH()
	const sha = "06b6e1c6f1c081c08ce935832631b4873ffe3882"
	f.set("api repos/"+repo+"/actions/runs?head_sha="+sha+"&per_page=50", fixture(t, "runs_same_second.json"), nil)
	runs, err := gh{r: f}.runs(context.Background(), repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].RunNumber != 507 || pickRun(runs) != 39137760005 {
		t.Errorf("runs %+v", runs)
	}
}

// A pr surface opened with the scope's fact reads the pull request from it, without
// asking pr.checks.
func TestRunsSurfaceFromFacts(t *testing.T) {
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	hs.setChecks(nil) // pr.checks would say "no pull request"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := hs.OpenSurfaceWith(ctx, pluginsdk.SurfaceOpen{Instance: inst, Surface: "runs", Context: pluginsdk.UiContext{ThreadID: "s-1"},
		Facts: map[string]json.RawMessage{"pr": json.RawMessage(`{"repo":"alexwaumann/code-foundry","number":197,"url":"https://github.com/alexwaumann/code-foundry/pull/197"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	hs.waitView(t, "the runs", func(v ui.View) bool { return node(v, "run-list") != nil })
	for _, inv := range hs.Invocations() {
		if inv.Command == "pr.checks" {
			t.Errorf("asked pr.checks although the facts name the pull request")
		}
	}
}

func TestRunsSurfaceStates(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(hs *harness)
		want  func(v ui.View) bool
	}{
		{"no runs yet", func(hs *harness) {
			realFailure(t, hs.gh)
			hs.gh.set("api repos/"+repo+"/actions/runs?head_sha="+headSHA+"&per_page=50", []byte(`{"total_count":0,"workflow_runs":[]}`), nil)
		}, func(v ui.View) bool {
			n := node(v, "none")
			return n != nil && n.Empty.Title == "No workflow runs for a6b1f0c yet"
		}},
		{"gh not signed in", func(hs *harness) {
			hs.gh.set("api repos/"+repo+"/pulls/197", nil, classify(errors.New("exit status 4"), "To get started with GitHub CLI, please run:  gh auth login\n"))
		}, func(v ui.View) bool {
			n := node(v, "error")
			return n != nil && n.Notice.Title == "Sign in to GitHub" && strings.Contains(n.Notice.Body.Text, "gh auth login")
		}},
		{"rate limited", func(hs *harness) {
			hs.gh.set("api repos/"+repo+"/pulls/197", nil, classify(errors.New("exit status 1"), "gh: API rate limit exceeded for user ID 1. (HTTP 403)"))
		}, func(v ui.View) bool {
			n := node(v, "error")
			return n != nil && n.Notice.Title == "GitHub rate limit reached"
		}},
		{"no gh", func(hs *harness) {
			hs.gh.set("api repos/"+repo+"/pulls/197", nil, classify(fmt.Errorf("exec: %w", errors.New("x")), ""))
			hs.gh.set("api repos/"+repo+"/pulls/197", nil, &ghError{Kind: errNoGH, Err: errors.New(`exec: "gh": executable file not found in $PATH`)})
		}, func(v ui.View) bool {
			n := node(v, "error")
			return n != nil && n.Notice.Title == "GitHub CLI not found"
		}},
		{"no pull request", func(hs *harness) { hs.setChecks(nil) }, func(v ui.View) bool {
			n := node(v, "none")
			return n != nil && n.Empty.Title == "No pull request"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hs := start(t, plugintest.Options{})
			tt.setup(hs)
			openRuns(t, hs)
			hs.waitView(t, tt.name, tt.want)
		})
	}
}

func TestRunsSurfaceStaleAfterError(t *testing.T) {
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	openRuns(t, hs)
	hs.waitView(t, "runs", func(v ui.View) bool { return node(v, "run-list") != nil })
	hs.gh.set("api repos/"+repo+"/actions/runs?head_sha="+headSHA+"&per_page=50", nil, classify(errors.New("exit status 1"), "gh: Server Error (HTTP 502)"))
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActClick, Action: "refresh"})
	v := hs.waitView(t, "the stale section", func(v ui.View) bool { return node(v, "stale") != nil })
	if s := node(v, "stale"); !s.Section.Stale || node(v, "run-list") == nil {
		t.Errorf("stale section %+v", s)
	}
	if n := node(v, "error"); n == nil || !strings.Contains(n.Notice.Detail, "HTTP 502") {
		t.Errorf("error notice %+v", n)
	}
}

// A run in progress: polled, and each job that fails meanwhile is appended to the log
// rather than re-sent.
func TestRunsSurfacePollsAndAppends(t *testing.T) {
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	runsKey := "api repos/" + repo + "/actions/runs?head_sha=" + headSHA + "&per_page=50"
	jobsKey := fmt.Sprintf("api repos/%s/actions/runs/%d/jobs?per_page=100", repo, failRun)
	now := time.Now().UTC()
	run := map[string]any{"id": failRun, "name": "ci", "run_number": 7, "run_attempt": 1, "event": "pull_request",
		"status": "in_progress", "html_url": "https://github.com/" + repo + "/actions/runs/39125565731",
		"created_at": now.Add(-time.Minute), "run_started_at": now.Add(-time.Minute), "updated_at": now, "head_sha": headSHA}
	job := func(id int64, name, status, conclusion string) map[string]any {
		j := map[string]any{"id": id, "run_id": failRun, "name": name, "status": status, "conclusion": conclusion,
			"started_at": now.Add(-time.Minute), "html_url": fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", repo, failRun, id)}
		if status == "completed" {
			j["completed_at"] = now
			j["steps"] = []map[string]any{{"name": "Run make " + name, "status": "completed", "conclusion": conclusion, "number": 3}}
		}
		return j
	}
	hs.gh.set(runsKey, mustJSON(map[string]any{"workflow_runs": []any{run}}), nil)
	hs.gh.set(jobsKey, mustJSON(map[string]any{"jobs": []any{job(1, "lint", "completed", "failure"), job(2, "test", "in_progress", "")}}), nil)
	hs.gh.set(fmt.Sprintf("run view --job 1 --log-failed -R %s", repo), []byte("lint\tRun make lint\t2026-10-18T12:00:00.0000000Z main.go:3: unused x\nlint\tRun make lint\t2026-10-18T12:00:01.0000000Z ##[error]Process completed with exit code 1.\n"), nil)
	openRuns(t, hs)
	v := hs.waitView(t, "cancel", func(v ui.View) bool { return buttons(v)["btn-cancel"].Key != "" && node(v, "run-list") != nil })
	if b := buttons(v)["btn-cancel"]; b.Style != ui.ButtonDestructive || b.Action.Command.Name != "plugin.gh-actions.cancel" {
		t.Errorf("cancel button %+v", b)
	}
	if v.Header.Pill.Label != "running" {
		t.Errorf("pill %+v", v.Header.Pill)
	}
	act(t, hs, pluginsdk.SurfaceAct{Kind: pluginsdk.ActTab, Action: "tab", Key: tabLog, Value: tabLog})
	v = hs.waitView(t, "lint's log", func(v ui.View) bool { n := logNode(v); return n != nil && len(n.Log.Lines) > 0 })
	if n := logNode(v); n.Log.Title != "lint · Run make lint" {
		t.Errorf("log title %q", n.Log.Title)
	}

	// test fails too: its lines arrive as an append, the view keeps the buffer.
	hs.gh.set(jobsKey, mustJSON(map[string]any{"jobs": []any{job(1, "lint", "completed", "failure"), job(2, "test", "completed", "failure")}}), nil)
	hs.gh.set(fmt.Sprintf("run view --job 2 --log-failed -R %s", repo), []byte("test\tRun make test\t2026-10-18T12:01:00.0000000Z --- FAIL: TestX (0.00s)\n"), nil)
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		for _, a := range h.Appends() {
			for _, l := range a.Lines {
				if l.Text == "--- FAIL: TestX (0.00s)" && l.Tone == "error" && a.Key == fmt.Sprintf("log-%d-1-all", failRun) {
					return true
				}
			}
		}
		return false
	})
	if n := logNode(hs.view(t)); n == nil || len(n.Log.Lines) != 0 {
		t.Errorf("the view re-sent the log instead of keeping the buffer: %+v", n)
	}
	if n := node(hs.view(t), "log-jobs"); n == nil || len(n.Chips.Chips) != 2 {
		t.Errorf("failed-job chips %+v", n)
	}

	// The run finishes: polling stops.
	run["status"], run["conclusion"] = "completed", "failure"
	hs.gh.set(runsKey, mustJSON(map[string]any{"workflow_runs": []any{run}}), nil)
	hs.waitView(t, "finished", func(v ui.View) bool { return v.Header.Pill != nil && v.Header.Pill.Label == "failed" })
	time.Sleep(50 * time.Millisecond)
	hs.gh.mu.Lock()
	n := len(hs.gh.calls)
	hs.gh.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	hs.gh.mu.Lock()
	defer hs.gh.mu.Unlock()
	if len(hs.gh.calls) != n {
		t.Errorf("still polling after the run finished: %v", hs.gh.calls[n:])
	}
}

func TestCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	hs.gh.set(fmt.Sprintf("run rerun %d -R %s --failed", failRun, repo), nil, nil)
	hs.gh.set(fmt.Sprintf("run rerun %d -R %s", failRun, repo), nil, nil)
	hs.gh.set(fmt.Sprintf("run cancel 42 -R %s", repo), nil, nil)
	uctx := pluginsdk.UiContext{ThreadID: "s-1"}

	for _, tt := range []struct {
		cmd  string
		args map[string]string
		want string // message, or the error's text
		call string // the gh call made
		code int
	}{
		{"rerun-failed", map[string]string{"thread": "s-1"}, "Rerunning the failed jobs of ci #494", fmt.Sprintf("run rerun %d -R %s --failed", failRun, repo), 0},
		{"rerun", map[string]string{"thread": "s-1"}, "Rerunning every job of ci #494", fmt.Sprintf("run rerun %d -R %s", failRun, repo), 0},
		{"cancel", map[string]string{"thread": "s-1"}, "no run in progress for alexwaumann/code-foundry#197 at a6b1f0c", "", pluginsdk.CodeUnavailable},
		{"cancel", map[string]string{"thread": "s-1", "run": "42"}, "Cancelling run 42", fmt.Sprintf("run cancel 42 -R %s", repo), 0},
		{"cancel", map[string]string{"thread": "s-1", "run": "x"}, `run "x" is not a run id`, "", pluginsdk.CodeInvalidParams},
		{"open", map[string]string{"thread": "s-1"}, "Opened https://github.com/alexwaumann/code-foundry/actions/runs/39125565731", "", 0},
	} {
		res, err := hs.Command(ctx, tt.cmd, tt.args, uctx)
		got := res.Message
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, tt.want) || pluginsdk.ErrorCode(err) != tt.code && !(tt.code == 0 && err == nil) {
			t.Errorf("%s %v = %q (code %d), want %q (code %d)", tt.cmd, tt.args, got, pluginsdk.ErrorCode(err), tt.want, tt.code)
		}
		if tt.call != "" && !hs.gh.called(tt.call) {
			t.Errorf("%s did not run gh %s", tt.cmd, tt.call)
		}
	}
	var opened string
	for _, inv := range hs.Invocations() {
		if inv.Command == "view.open.url" {
			opened = inv.Args["url"]
		}
	}
	if opened != "https://github.com/alexwaumann/code-foundry/actions/runs/39125565731" {
		t.Errorf("view.open.url %q", opened)
	}

	// No thread, and a thread without a pull request.
	if _, err := hs.Command(ctx, "rerun", nil, pluginsdk.UiContext{}); pluginsdk.ErrorCode(err) != pluginsdk.CodeInvalidParams {
		t.Errorf("no thread: %v", err)
	}
	hs.setChecks(nil)
	if _, err := hs.Command(ctx, "open", map[string]string{"thread": "s-1"}, uctx); pluginsdk.ErrorCode(err) != pluginsdk.CodeUnavailable {
		t.Errorf("no pull request: %v", err)
	}
}

const branch = "cf/plugins-4-skill" // #197's head

func threadsSnapshot(ids ...string) pluginsdk.Event {
	var ts []map[string]string
	for _, id := range ids {
		ts = append(ts, map[string]string{"id": id, "status": "THREAD_STATUS_IDLE", "branch": branch})
	}
	return pluginsdk.Event{Subscription: "sub-1", Snapshot: true,
		Event: mustJSON(map[string]any{"threadList": map[string]any{"snapshot": map[string]any{"threads": ts}}, "epoch": "e1"})}
}

// threadUpdated is a threadList.updated for a thread on branch b, linking the pull
// requests given (owner/name#n).
func threadUpdated(id, b string, linked ...int) pluginsdk.Event {
	var prs []map[string]any
	for _, n := range linked {
		prs = append(prs, map[string]any{"slug": repo, "number": n, "url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)})
	}
	return pluginsdk.Event{Subscription: "sub-1", Event: mustJSON(map[string]any{"threadList": map[string]any{"updated": map[string]any{
		"id": id, "branch": b, "status": "THREAD_STATUS_BUSY", "lastActivityAt": time.Now().Format(time.RFC3339Nano), "linkedPullRequests": prs}},
		"seq": "5", "epoch": "e1"})}
}

// prEvent is a gh.pullRequestChanged for pull request n of repo on head ref, its state
// after the change state (OPEN, MERGED, CLOSED), with changes of the kinds given
// (CHECKS, HEAD_PUSHED, …).
func prEvent(n int, ref, state string, kinds ...string) pluginsdk.Event {
	var changes []map[string]any
	for _, k := range kinds {
		changes = append(changes, map[string]any{"kind": "PULL_REQUEST_CHANGE_KIND_" + k})
	}
	url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
	return pluginsdk.Event{Subscription: "sub-1", Event: mustJSON(map[string]any{"gh": map[string]any{"pullRequestChanged": map[string]any{
		"repoSlug": repo, "number": n, "url": url, "headRef": ref,
		"pullRequest": map[string]any{"repoSlug": repo, "number": n, "headRef": ref, "url": url, "state": "PULL_REQUEST_STATE_" + state},
		"changes":     changes}}, "seq": "12", "epoch": "e1"})}
}

// ghSnapshot is one event of the gh snapshot the host resends after the plugin fell
// behind (the snapshot is notifications flagged Snapshot, not a gh.snapshot).
var ghSnapshot = pluginsdk.Event{Subscription: "sub-1", Snapshot: true, Event: json.RawMessage(`{"gh":{"polled":{"fetchedAt":"2026-10-18T12:26:35Z"}},"epoch":"e1"}`)}

// checksReads counts the pr.checks calls the plugin made, per thread.
func checksReads(h *plugintest.Host, thread string) int {
	n := 0
	for _, inv := range h.Invocations() {
		if inv.Command == "pr.checks" && inv.Args["thread"] == thread {
			n++
		}
	}
	return n
}

// settle waits for the debounce and the checks it starts.
func settle() { time.Sleep(100 * time.Millisecond) }

var ghPolled = pluginsdk.Event{Subscription: "sub-1", Event: json.RawMessage(`{"gh":{"polled":{"fetchedAt":"2026-10-18T12:26:35Z"}},"seq":"9","epoch":"e1"}`)}

func flagsFor(h *plugintest.Host, thread string) []pluginsdk.Flag {
	var out []pluginsdk.Flag
	for _, f := range h.Flags() {
		if f.ThreadID == thread {
			out = append(out, f)
		}
	}
	return out
}

// A failed run flags the thread once; a later passing run clears it; a restart does
// not flag the same failure again.
func TestFlagLifecycle(t *testing.T) {
	dir := t.TempDir()
	hs := start(t, plugintest.Options{DataDir: dir})
	if subs := hs.Subscriptions(); len(subs) != 1 || strings.Join(subs[0], ",") != "threads,gh" {
		t.Fatalf("subscriptions %v", subs)
	}
	if err := hs.Event(threadsSnapshot("s-1")); err != nil {
		t.Fatal(err)
	}
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(flagsFor(h, "s-1")) == 1 })
	f := flagsFor(hs.Host, "s-1")[0]
	if f.Level != pluginsdk.FlagAttention || f.Reason != "go-test failed" || !f.UntilSeen {
		t.Errorf("flag %+v", f)
	}
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		for _, c := range h.Contexts() {
			if c.Key == "failing" && c.ThreadID == "s-1" && c.Value == true {
				return true
			}
		}
		return false
	})

	// The same failure again (another poll's checks event): no second flag.
	checksMoved := prEvent(197, branch, "OPEN", "CHECKS")
	if err := hs.Event(checksMoved); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(flagsFor(hs.Host, "s-1")); n != 1 {
		t.Errorf("%d flags after the same failure", n)
	}

	// A rerun that passes clears it.
	hs.setChecks(checksJSON(nil))
	if err := hs.Event(checksMoved); err != nil {
		t.Fatal(err)
	}
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		fs := flagsFor(h, "s-1")
		return len(fs) == 2 && fs[1].Level == pluginsdk.FlagNone
	})

	// A run in progress leaves things alone; a new failure flags again.
	hs.setChecks(checksJSON(map[string]string{"go-test": ""}))
	_ = hs.Event(checksMoved)
	time.Sleep(50 * time.Millisecond)
	hs.setChecks(checksJSON(map[string]string{"go-lint": "failure"}))
	_ = hs.Event(checksMoved)
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		fs := flagsFor(h, "s-1")
		return len(fs) == 3 && fs[2].Reason == "go-lint failed"
	})
	// The state is saved just after SetFlag returns: wait for it before restarting.
	hs.WaitFor(5*time.Second, func(*plugintest.Host) bool {
		data, _ := os.ReadFile(filepath.Join(dir, "flags.json"))
		return strings.Contains(string(data), "/117669858971")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// After a restart the same failure is not flagged again.
	hs2 := start(t, plugintest.Options{DataDir: dir})
	hs2.setChecks(checksJSON(map[string]string{"go-lint": "failure"}))
	_ = hs2.Event(threadsSnapshot("s-1"))
	hs2.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		for _, inv := range h.Invocations() {
			if inv.Command == "pr.checks" {
				return true
			}
		}
		return false
	})
	time.Sleep(100 * time.Millisecond)
	if fs := flagsFor(hs2.Host, "s-1"); len(fs) != 0 {
		t.Errorf("flagged again after a restart: %+v", fs)
	}
}

// Threads without a pull request are left alone.
func TestFlagNoPullRequest(t *testing.T) {
	hs := start(t, plugintest.Options{})
	hs.setChecks(nil)
	_ = hs.Event(threadsSnapshot("s-2"))
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(h.Invocations()) > 0 })
	time.Sleep(50 * time.Millisecond)
	if fs := hs.Flags(); len(fs) != 0 {
		t.Errorf("flags %+v", fs)
	}
}

func TestParseFailedLog(t *testing.T) {
	lines := parseFailedLog(fixture(t, "log_failed.txt"))
	if len(lines) < 50 {
		t.Fatalf("%d lines", len(lines))
	}
	if lines[0].Job != "go-test" || lines[0].Step != "Run make go-test" || lines[0].Text != "##[group]Run make go-test" {
		t.Errorf("first line %+v", lines[0])
	}
	if lines[1].Text != "make go-test" {
		t.Errorf("colour codes left: %q", lines[1].Text)
	}
	for _, l := range lines {
		if l.Text == "##[endgroup]" {
			t.Error("endgroup kept")
		}
	}
}

func TestToneOf(t *testing.T) {
	for _, tt := range []struct {
		in, out string
		tone    ui.Tone
	}{
		{"##[error]Process completed with exit code 2.", "Error: Process completed with exit code 2.", ui.ToneError},
		{"##[warning]Node 16 is deprecated", "Warning: Node 16 is deprecated", ui.ToneAttention},
		{"##[group]Run make go-test", "Run make go-test", ui.ToneMuted},
		{"--- FAIL: TestX (0.00s)", "--- FAIL: TestX (0.00s)", ui.ToneError},
		{"FAIL\tgithub.com/x/y\t1.2s", "FAIL\tgithub.com/x/y\t1.2s", ui.ToneError},
		{"panic: boom", "panic: boom", ui.ToneError},
		{"make: *** [go-test] Error 1", "make: *** [go-test] Error 1", ui.ToneError},
		{"ok  \tgithub.com/x/y\t1.2s", "ok  \tgithub.com/x/y\t1.2s", ui.ToneNeutral},
	} {
		out, tone := toneOf(tt.in)
		if out != tt.out || tone != tt.tone {
			t.Errorf("toneOf(%q) = %q, %q; want %q, %q", tt.in, out, tone, tt.out, tt.tone)
		}
	}
}

func TestSummarize(t *testing.T) {
	for _, tt := range []struct {
		name           string
		checks         map[string]string
		failed, passed bool
		job            string
	}{
		{"all passed", nil, false, true, ""},
		{"one failed", map[string]string{"go-test": "failure"}, true, false, "go-test"},
		{"timed out", map[string]string{"go-lint": "timed_out"}, true, false, "go-lint"},
		{"in progress", map[string]string{"go-test": ""}, false, false, ""},
		{"skipped and cancelled", map[string]string{"go-test": "skipped", "frontend": "cancelled"}, false, true, ""},
	} {
		var pc prChecks
		if err := json.Unmarshal(checksJSON(tt.checks), &pc); err != nil {
			t.Fatal(err)
		}
		st := summarize(pc)
		if st.Failed != tt.failed || st.Passed != tt.passed || st.Job != tt.job {
			t.Errorf("%s: %+v", tt.name, st)
		}
	}
}

func TestFailureExcerptLimit(t *testing.T) {
	var lines []logLine
	for i := 0; i < 500; i++ {
		lines = append(lines, logLine{Job: "test", Step: "Run tests", Text: fmt.Sprintf("line %d %s", i, strings.Repeat("x", 200))})
	}
	text := failureExcerpt(ghRun{Name: "ci", RunNumber: 3, HTMLURL: "https://github.com/o/r/actions/runs/1"},
		ghJob{Name: "test", HTMLURL: "https://github.com/o/r/actions/runs/1/job/2", Steps: []ghStep{{Name: "Run tests", Conclusion: "failure"}}}, lines,
		"code-foundry thread subscribe --plugin gh-actions/run-finished --arg pr=o/r#1")
	if len(text) > 4096 {
		t.Errorf("%d bytes", len(text))
	}
	if !strings.HasSuffix(text, "\ncode-foundry thread subscribe --plugin gh-actions/run-finished --arg pr=o/r#1") {
		t.Errorf("the excerpt does not end with the wake hint:\n%s", text[len(text)-300:])
	}
	if !strings.Contains(text, "line 499") || strings.Contains(text, "line 439 ") || strings.Count(text, "\n") > 75 {
		t.Errorf("excerpt is not the tail:\n%s", text)
	}
}

func TestDecodePullRequestChanged(t *testing.T) {
	for _, tt := range []struct {
		name          string
		event         []byte
		key, ref      string
		relevant      bool
		closed, moved bool // moved: a push or new checks (pokes the panel)
	}{
		{"captured checks", fixture(t, "pull_request_changed.json"), "alexwaumann/code-foundry#210", "cf/gh-events-1-pull-request-changed", true, false, true},
		{"head pushed", prEvent(197, branch, "OPEN", "HEAD_PUSHED", "CHECKS").Event, "alexwaumann/code-foundry#197", branch, true, false, true},
		{"appeared", prEvent(197, branch, "OPEN", "APPEARED").Event, "alexwaumann/code-foundry#197", branch, true, false, false},
		{"merged", prEvent(197, branch, "MERGED", "MERGED").Event, "alexwaumann/code-foundry#197", branch, true, true, false},
		{"closed", prEvent(197, branch, "CLOSED", "CLOSED").Event, "alexwaumann/code-foundry#197", branch, true, true, false},
		{"reopened", prEvent(197, branch, "OPEN", "REOPENED").Event, "alexwaumann/code-foundry#197", branch, true, false, false},
		{"reviews and comments", prEvent(197, branch, "OPEN", "REVIEWS", "COMMENTS", "REVIEW_DECISION").Event, "alexwaumann/code-foundry#197", branch, false, false, false},
		{"title and draft", prEvent(197, branch, "OPEN", "DRAFT", "TITLE").Event, "alexwaumann/code-foundry#197", branch, false, false, false},
	} {
		var e ghEvent
		if err := json.Unmarshal(tt.event, &e); err != nil || e.GH == nil || e.GH.PullRequestChanged == nil {
			t.Fatalf("%s: %v %s", tt.name, err, tt.event)
		}
		pr := e.GH.PullRequestChanged
		if got := prKey(pr.RepoSlug, pr.Number); got != tt.key || pr.HeadRef != tt.ref {
			t.Errorf("%s: %s on %s, want %s on %s", tt.name, got, pr.HeadRef, tt.key, tt.ref)
		}
		if pr.flagRelevant() != tt.relevant || pr.isClosed() != tt.closed || pr.has(kindChecks, kindHeadPushed) != tt.moved {
			t.Errorf("%s: relevant %v closed %v moved %v", tt.name, pr.flagRelevant(), pr.isClosed(), pr.has(kindChecks, kindHeadPushed))
		}
	}
}

// Which events make the watcher re-read a thread's checks, and what the flag does then.
// Each case starts from thread s-1 on #197's branch, read once with its checks running
// (or without a pull request), then changes the checks and sends the events.
func TestFlagFollowsEvents(t *testing.T) {
	failing := checksJSON(map[string]string{"go-test": "failure"})
	for _, tt := range []struct {
		name    string
		before  []byte // pr.checks at the start (nil: no pull request)
		setup   []pluginsdk.Event
		after   []byte
		events  []pluginsdk.Event
		reads   int    // pr.checks reads after the setup's
		flagged string // the flag's reason, "" for none
	}{
		{"checks finished failing", checksJSON(map[string]string{"go-test": ""}), nil, failing,
			[]pluginsdk.Event{prEvent(197, branch, "OPEN", "CHECKS")}, 1, "go-test failed"},
		{"head pushed", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{prEvent(197, branch, "OPEN", "HEAD_PUSHED")}, 1, "go-test failed"},
		{"a burst is one read", checksJSON(map[string]string{"go-test": ""}), nil, failing,
			[]pluginsdk.Event{prEvent(197, branch, "OPEN", "HEAD_PUSHED", "CHECKS"), prEvent(197, branch, "OPEN", "CHECKS")}, 1, "go-test failed"},
		{"appeared on the thread's branch", nil, nil, failing,
			[]pluginsdk.Event{prEvent(197, branch, "OPEN", "APPEARED")}, 1, "go-test failed"},
		{"appeared, linked by the thread", nil, []pluginsdk.Event{threadUpdated("s-1", "elsewhere", 197)}, failing,
			[]pluginsdk.Event{prEvent(197, "other-head", "OPEN", "APPEARED")}, 1, "go-test failed"},
		{"unknown pull request", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{prEvent(5, "someone-else", "OPEN", "CHECKS", "HEAD_PUSHED")}, 0, ""},
		{"reviews only", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{prEvent(197, branch, "OPEN", "REVIEWS", "REVIEW_DECISION")}, 0, ""},
		{"other gh events", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{ghPolled, {Subscription: "sub-1", Event: json.RawMessage(`{"gh":{"pullRequestDetailUpdated":{"repoSlug":"` + repo + `","number":197}},"seq":"10","epoch":"e1"}`)}}, 0, ""},
		{"thread busy, same branch", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{threadUpdated("s-1", branch)}, 0, ""},
		{"thread on a new branch", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{threadUpdated("s-1", "cf/next")}, 1, "go-test failed"},
		{"new thread", checksJSON(nil), nil, failing,
			[]pluginsdk.Event{threadUpdated("s-9", branch)}, 0, ""}, // s-9's read, not s-1's
	} {
		t.Run(tt.name, func(t *testing.T) {
			hs := start(t, plugintest.Options{})
			hs.setChecks(tt.before)
			_ = hs.Event(threadsSnapshot("s-1"))
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return checksReads(h, "s-1") == 1 })
			for _, ev := range tt.setup {
				_ = hs.Event(ev)
			}
			settle()
			base := checksReads(hs.Host, "s-1")
			hs.setChecks(tt.after)
			for _, ev := range tt.events {
				if err := hs.Event(ev); err != nil {
					t.Fatal(err)
				}
			}
			if tt.flagged != "" {
				hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(flagsFor(h, "s-1")) == 1 })
			}
			settle()
			if n := checksReads(hs.Host, "s-1") - base; n != tt.reads {
				t.Errorf("%d reads, want %d", n, tt.reads)
			}
			fs := flagsFor(hs.Host, "s-1")
			switch {
			case tt.flagged == "" && len(fs) != 0:
				t.Errorf("flagged %+v", fs)
			case tt.flagged != "" && (len(fs) != 1 || fs[0].Reason != tt.flagged):
				t.Errorf("flags %+v, want %q", fs, tt.flagged)
			}
			if tt.name == "new thread" && checksReads(hs.Host, "s-9") != 1 {
				t.Errorf("the new thread was read %d times", checksReads(hs.Host, "s-9"))
			}
		})
	}
}

// Nothing is read on a timer: with no events, no pr.checks.
func TestNoReadsWithoutEvents(t *testing.T) {
	hs := start(t, plugintest.Options{}, func(a *app) { a.retryMin, a.retryMax = 10*time.Millisecond, 10*time.Millisecond })
	hs.setChecks(checksJSON(map[string]string{"go-test": ""}))
	_ = hs.Event(threadsSnapshot("s-1"))
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return checksReads(h, "s-1") == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := checksReads(hs.Host, "s-1"); n != 1 {
		t.Errorf("%d reads without an event", n)
	}
}

// A merged or closed pull request lowers its threads' flags and does not raise them
// again for the same failure, also after a resync.
func TestFlagClearedOnMergeOrClose(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			hs := start(t, plugintest.Options{})
			_ = hs.Event(threadsSnapshot("s-1"))
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(flagsFor(h, "s-1")) == 1 })
			// pr.checks still answers with the failing checks: the branch's pull request is
			// the merged one.
			_ = hs.Event(prEvent(197, branch, state, state))
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
				fs := flagsFor(h, "s-1")
				return len(fs) == 2 && fs[1].Level == pluginsdk.FlagNone
			})
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
				for _, c := range h.Contexts() {
					if c.Key == "failing" && c.ThreadID == "s-1" && c.Value == nil {
						return true
					}
				}
				return false
			})
			_ = hs.Event(ghSnapshot)
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return checksReads(h, "s-1") == 3 })
			settle()
			if fs := flagsFor(hs.Host, "s-1"); len(fs) != 2 {
				t.Errorf("flags after the resync %+v", fs)
			}
			// Reopened: its failure is the thread's again, but it was flagged already.
			_ = hs.Event(prEvent(197, branch, "OPEN", "REOPENED"))
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return checksReads(h, "s-1") == 4 })
			settle()
			if fs := flagsFor(hs.Host, "s-1"); len(fs) != 2 {
				t.Errorf("flags after reopening %+v", fs)
			}
		})
	}
}

// The gh snapshot the host resends after the plugin fell behind re-reads every thread
// once (events may have been lost).
func TestSnapshotRechecksEveryThread(t *testing.T) {
	hs := start(t, plugintest.Options{})
	hs.setChecks(checksJSON(nil))
	_ = hs.Event(threadsSnapshot("s-1", "s-2", "s-3"))
	reads := func(want int) func(*plugintest.Host) bool {
		return func(h *plugintest.Host) bool {
			return checksReads(h, "s-1") == want && checksReads(h, "s-2") == want && checksReads(h, "s-3") == want
		}
	}
	hs.WaitFor(5*time.Second, reads(1))
	// The snapshot is several events (viewer, last poll, dashboard, …): one re-read.
	for _, kind := range []string{"viewerUpdated", "polled", "dashboardUpdated"} {
		_ = hs.Event(pluginsdk.Event{Subscription: "sub-1", Snapshot: true, Event: json.RawMessage(`{"gh":{"` + kind + `":{}},"epoch":"e1"}`)})
	}
	hs.WaitFor(5*time.Second, reads(2))
	_ = hs.Event(ghPolled) // a live event is not a resync
	settle()
	if !reads(2)(hs.Host) {
		t.Errorf("reads %d %d %d, want 2 each", checksReads(hs.Host, "s-1"), checksReads(hs.Host, "s-2"), checksReads(hs.Host, "s-3"))
	}
}

// After a rerun nothing polls for the flag: the rollup's events, pending and then
// failed, flag the new attempt's failure with no panel open.
func TestRerunFlaggedByEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := start(t, plugintest.Options{})
	realFailure(t, hs.gh)
	hs.gh.set(fmt.Sprintf("run rerun %d -R %s --failed", failRun, repo), nil, nil)
	_ = hs.Event(threadsSnapshot("s-1"))
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(flagsFor(h, "s-1")) == 1 })
	if _, err := hs.Command(ctx, "rerun-failed", map[string]string{"thread": "s-1"}, pluginsdk.UiContext{ThreadID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	settle()
	before := checksReads(hs.Host, "s-1")
	hs.setChecks(checksJSON(map[string]string{"go-test": ""}))
	_ = hs.Event(prEvent(197, branch, "OPEN", "CHECKS")) // failure → pending
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return checksReads(h, "s-1") == before+1 })
	settle()
	hs.setChecks(checksJSON(map[string]string{"go-lint": "failure"}))
	_ = hs.Event(prEvent(197, branch, "OPEN", "CHECKS")) // pending → failure
	hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool {
		fs := flagsFor(h, "s-1")
		return len(fs) == 2 && fs[1].Reason == "go-lint failed"
	})
}

// A read that failed (or got the host's stale copy) is retried with a backoff: the event
// that asked for it will not come again.
func TestRetryAfterFailedRead(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail func(hs *harness)
	}{
		{"error", func(hs *harness) {
			hs.mu.Lock()
			hs.checksErr = pluginsdk.NewError(pluginsdk.CodeInternal, "GitHub is down")
			hs.mu.Unlock()
		}},
		{"stale copy", func(hs *harness) {
			var pc map[string]any
			_ = json.Unmarshal(checksJSON(map[string]string{"go-test": ""}), &pc)
			pc["lastError"] = "HTTP 502"
			hs.setChecks(mustJSON(pc))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hs := start(t, plugintest.Options{}, func(a *app) { a.retryMin, a.retryMax = 30*time.Millisecond, 60*time.Millisecond })
			tt.fail(hs)
			_ = hs.Event(threadsSnapshot("s-1"))
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return checksReads(h, "s-1") >= 2 })
			hs.mu.Lock()
			hs.checksErr = nil
			hs.checks = checksJSON(map[string]string{"go-test": "failure"})
			hs.mu.Unlock()
			hs.WaitFor(5*time.Second, func(h *plugintest.Host) bool { return len(flagsFor(h, "s-1")) == 1 })
			n := checksReads(hs.Host, "s-1")
			time.Sleep(200 * time.Millisecond)
			if m := checksReads(hs.Host, "s-1"); m != n {
				t.Errorf("still retrying after a good read: %d → %d", n, m)
			}
		})
	}
}
