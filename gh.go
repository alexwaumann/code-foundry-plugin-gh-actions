package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ghRunner runs the GitHub CLI. The plugin uses the user's own `gh` login; tests
// swap in a fake.
type ghRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// execGH runs the real `gh` from PATH.
type execGH struct{}

func (execGH) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1")
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), classify(err, stdout.String()+stderr.String())
	}
	return stdout.Bytes(), nil
}

// errKind says what went wrong talking to GitHub, for the surface's notices.
type errKind int

const (
	errOther errKind = iota
	errNoGH
	errAuth
	errRateLimit
	errNotFound
)

// ghError is a failed gh call with its output.
type ghError struct {
	Kind   errKind
	Output string
	Err    error
}

func (e *ghError) Error() string {
	out := strings.TrimSpace(e.Output)
	if out == "" {
		return e.Err.Error()
	}
	if i := strings.IndexByte(out, '\n'); i > 0 {
		out = out[:i]
	}
	return out
}

func (e *ghError) Unwrap() error { return e.Err }

// classify turns a gh failure into a ghError by its output.
func classify(err error, output string) *ghError {
	e := &ghError{Kind: errOther, Output: output, Err: err}
	low := strings.ToLower(output)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		e.Kind = errNoGH
	case strings.Contains(low, "rate limit"):
		e.Kind = errRateLimit
	case strings.Contains(low, "gh auth login"), strings.Contains(low, "http 401"),
		strings.Contains(low, "not logged in"), strings.Contains(low, "bad credentials"):
		e.Kind = errAuth
	case strings.Contains(low, "http 404"), strings.Contains(low, "not found"):
		e.Kind = errNotFound
	}
	return e
}

func kindOf(err error) errKind {
	var ge *ghError
	if errors.As(err, &ge) {
		return ge.Kind
	}
	return errOther
}

// ghRun is one workflow run (GET /repos/{repo}/actions/runs).
type ghRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	RunNumber    int       `json:"run_number"`
	RunAttempt   int       `json:"run_attempt"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"run_started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	HeadSHA      string    `json:"head_sha"`
}

// ghJob is one job of a run (GET /repos/{repo}/actions/runs/{id}/jobs).
type ghJob struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	HTMLURL     string    `json:"html_url"`
	Steps       []ghStep  `json:"steps"`
}

type ghStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int    `json:"number"`
}

// ghPull is what the surface needs of a pull request.
type ghPull struct {
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
}

// gh wraps a ghRunner with the calls the plugin makes.
type gh struct{ r ghRunner }

func (g gh) pull(ctx context.Context, repo string, number int) (ghPull, error) {
	var p ghPull
	out, err := g.r.Run(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, number))
	if err != nil {
		return p, err
	}
	return p, decode(out, &p)
}

// run reads one workflow run.
func (g gh) run(ctx context.Context, repo string, id int64) (ghRun, error) {
	var r ghRun
	out, err := g.r.Run(ctx, "api", fmt.Sprintf("repos/%s/actions/runs/%d", repo, id))
	if err != nil {
		return r, err
	}
	return r, decode(out, &r)
}

// runs lists the workflow runs of a commit, newest first (sortRuns).
func (g gh) runs(ctx context.Context, repo, sha string) ([]ghRun, error) {
	var res struct {
		WorkflowRuns []ghRun `json:"workflow_runs"`
	}
	out, err := g.r.Run(ctx, "api", fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=50", repo, sha))
	if err != nil {
		return nil, err
	}
	if err := decode(out, &res); err != nil {
		return nil, err
	}
	sortRuns(res.WorkflowRuns)
	return res.WorkflowRuns, nil
}

// jobs lists the jobs of a run's latest attempt.
func (g gh) jobs(ctx context.Context, repo string, run int64) ([]ghJob, error) {
	var res struct {
		Jobs []ghJob `json:"jobs"`
	}
	out, err := g.r.Run(ctx, "api", fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", repo, run))
	if err != nil {
		return nil, err
	}
	if err := decode(out, &res); err != nil {
		return nil, err
	}
	return res.Jobs, nil
}

// failedLog is the failed steps' log of one finished job, as `gh run view --log-failed`
// prints it.
func (g gh) failedLog(ctx context.Context, repo string, job int64) ([]logLine, error) {
	out, err := g.r.Run(ctx, "run", "view", "--job", strconv.FormatInt(job, 10), "--log-failed", "-R", repo)
	if err != nil {
		return nil, err
	}
	return parseFailedLog(out), nil
}

func (g gh) rerun(ctx context.Context, repo string, run int64, failedOnly bool) error {
	args := []string{"run", "rerun", strconv.FormatInt(run, 10), "-R", repo}
	if failedOnly {
		args = append(args, "--failed")
	}
	_, err := g.r.Run(ctx, args...)
	return err
}

func (g gh) cancel(ctx context.Context, repo string, run int64) error {
	_, err := g.r.Run(ctx, "run", "cancel", strconv.FormatInt(run, 10), "-R", repo)
	return err
}

func decode(out []byte, v any) error {
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("reading GitHub's answer: %w", err)
	}
	return nil
}

// logLine is one line of a failed step's log: the job and step it came from and the
// text without its timestamp or colour codes.
type logLine struct {
	Job, Step, Text string
}

var (
	ansiRe      = regexp.MustCompile(`(\x1b|\^\[)\[[0-9;?]*[A-Za-z]`)
	timestampRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)
)

// parseFailedLog reads `gh run view --log-failed` output: "job\tstep\ttimestamp text"
// per line. gh writes the escape character of colour codes as a literal "^[" when not
// on a terminal; both forms are stripped.
func parseFailedLog(out []byte) []logLine {
	var lines []logLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		text := strings.TrimPrefix(parts[2], "\ufeff")
		text = timestampRe.ReplaceAllString(text, "")
		text = ansiRe.ReplaceAllString(text, "")
		text = strings.TrimRight(text, "\r ")
		if text == "##[endgroup]" {
			continue
		}
		lines = append(lines, logLine{Job: parts[0], Step: parts[1], Text: text})
	}
	return lines
}

// runIDFromURL reads the run id out of an Actions check URL
// (https://github.com/o/r/actions/runs/<run>/job/<job>); 0 if it is not one.
func runIDFromURL(u string) (run, job int64) {
	const marker = "/actions/runs/"
	i := strings.Index(u, marker)
	if i < 0 {
		return 0, 0
	}
	rest := strings.Split(u[i+len(marker):], "/")
	run, _ = strconv.ParseInt(rest[0], 10, 64)
	if len(rest) >= 3 && rest[1] == "job" {
		job, _ = strconv.ParseInt(rest[2], 10, 64)
	}
	return run, job
}
