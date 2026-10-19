# code-foundry-plugin-gh-actions

A [Code Foundry](https://github.com/alexwaumann/code-foundry) plugin that tracks the
GitHub Actions runs of a thread's pull request: what ran on its head commit, which job
failed at which step, the failed step's log, and the buttons to rerun, cancel, or hand
the failure to Claude. It flags the thread when a run fails, and agents wait for a run to
finish with its `run-finished` wake source instead of polling.

```
code-foundry plugin install gh-actions --enable
code-foundry plugin install alexwaumann/code-foundry-plugin-gh-actions@v0.2.0 --enable   # a release directly
```

## What it shows

An **Actions** tab in the side panel of any thread whose pull request has GitHub Actions
checks (one panel per pull request, shared by the threads on it):

- **Header**: the pull request's number and title, the selected run's workflow, a status
  pill (passed, failed, running, queued, cancelled, …), how long it took or a live
  elapsed time, the head commit, and buttons to open the run on GitHub and refresh.
- **Runs**: every workflow run of the pull request's head commit, newest first (runs
  created in the same second: the higher run id first), with its status, run number and
  attempt, event, and duration. The one selected at first is the newest that failed or
  is still going, else the newest. Click one to see its jobs.
- **Jobs**: the selected run's jobs with status and duration; a failed job names the step
  that failed. Click one to see its log.
- **Log**: the failed steps' log tail (the last 200 lines of each failed job, timestamps
  and colour codes stripped, error lines red, warnings amber). While a run is still going,
  each job that fails is appended as it finishes.
- **Actions**: *Send failure to Claude* sends the thread you are looking at a request to
  investigate, with the workflow, job, step, run URL and up to 60 log lines (the error
  lines and the last lines, at most 4 KB). The panel is shared by every thread on the pull
  request, so the button runs the `send-failure` command, which the app runs with the
  viewing panel's thread. *Rerun failed* and *Rerun all* for a finished run, *Cancel*
  (asks first) for one in progress.

The panel polls GitHub every 15 s while a run is in progress and someone looks, every
60 s while hidden, and stops once every run has finished (a rerun, a cancel, or new
checks wake it). Empty and error states: no runs yet for the head commit, `gh` missing or
not signed in (with the `gh auth login` hint), a rate limit, and other API errors (the
last data stays, dimmed).

**Flag.** When a GitHub Actions run of a thread's pull request fails, the thread gets an
attention flag, "GitHub Actions: <job> failed", until you open it. The same failure is
not flagged again (also across restarts); a later failure is. A later run that passes
clears it. The fact `gh-actions.failing` is set on such a thread for `when` clauses.

**Wake source** `run-finished` (needs Code Foundry with wake sources, pluginsdk v0.2.0):
an agent's thread runs

```
code-foundry thread subscribe --plugin gh-actions/run-finished [--arg run=<run id>] [--repeat]
```

and is woken when a GitHub Actions run on its pull request's head commit completes (`pr`
defaults to the thread's pull request, `owner/name#123`; `run` waits for one run only).
Each run attempt fires once, with a title like `ci #507 passed` or `ci #494 failed
(attempt 2)` and the payload `{pr, run, name, number, attempt, event, status, conclusion,
url, head_sha, failed_jobs: [{id, name, step, url}], still_running}`. Runs that had
already finished when the subscription came are not news. While a pull request has a
subscription the plugin polls its runs every 20 s while one is going (or none started
yet) and every 2 minutes once all finished; an open panel's fetches and new checks seen
by the flag watcher count too. It stops polling with the pull request's last
subscription (a one-shot goes after its first wake); the panel keeps its own polling
while someone looks.

Every message about a run ends with the line that subscribes to it: the text *Send
failure to Claude* sends, the `rerun-failed`, `rerun` and `cancel` results (with `--arg
run=<id>`), and the panel's notice under a failed or running run. The flag reason does
not: the app caps it at 80 characters, too short for the line.

**Commands** (palette under *GitHub Actions*, CLI `code-foundry plugin gh-actions …`),
each for the active thread or `--thread <id>`, optionally `--run <id>` (or the run id as
a word):

| Command | Does |
|---|---|
| `rerun-failed` | Rerun the failed jobs of the newest failed (or cancelled) run |
| `rerun` | Rerun every job of the newest finished run |
| `cancel` | Cancel the run in progress (asks first; `--yes` on the CLI) |
| `open` | Open the run on GitHub: the one in progress, else the failed one, else the newest |
| `send-failure` | Send the newest failed run's (or `--run`'s) failed step log to Claude in the thread; `--job <id>` picks the job |

## Permissions and why

| Permission | Why |
|---|---|
| commands `pr.checks` | Find the thread's pull request and its checks (the flag, and the panel's pull request when it is opened from the palette) |
| commands `thread.send` | *Send failure to Claude* (`send-failure`) |
| commands `view.open.url` | The `open` command opens the run in your browser |
| events `threads`, `gh` | Know the threads and when GitHub polling saw new pull requests or checks, to flag failures |
| `threadStatus` | The attention flag on a thread whose run failed |

No secrets, no session environment, no API access. GitHub is reached through your own
`gh` login (`gh api`, `gh run view --log-failed`, `gh run rerun`, `gh run cancel`); the
plugin keeps one file, the failures it already flagged, in its data folder.

## Development

Needs Go 1.24+ and a Code Foundry with plugins.

```
make build                                     # bin/gh-actions
code-foundry plugin dev "$PWD" --follow=false --yes
code-foundry plugin list --context-thread <thread>      # runs(on) where the PR has Actions checks
code-foundry plugin surface gh-actions runs --thread <thread>   # the view and what acts (run twice: the first is "Loading")
code-foundry plugin act gh-actions runs tab --key tab-jobs --thread <thread>
code-foundry plugin act gh-actions runs --key run-<run id> --thread <thread>
code-foundry plugin gh-actions send-failure [<run id>] --thread <thread>   # what the button runs
code-foundry plugin gh-actions open --thread <thread>
code-foundry plugin wakes gh-actions                     # the run-finished line
code-foundry thread subscribe --id <thread> --plugin gh-actions/run-finished
code-foundry plugin logs gh-actions --lines 50          # "run-finished armed", "… fired"
make build && code-foundry plugin restart gh-actions   # after every change
make test
```

0.2.0 was tested on 2026-10-18 against a dev Code Foundry (363eda43) in an isolated
`CODE_FOUNDRY_HOME`: `plugin wakes gh-actions` listed `run-finished`; a thread subscribed
with `--arg pr=alexwaumann/code-foundry#216` and `plugin logs gh-actions` showed `run-finished
armed`, a poll (`head=fac70ae runs=1 running=0 next=2m0s`: every run had finished) and,
after `thread unsubscribe`, `run-finished stopped`. Fires are covered by the unit tests
(no reruns on that repository).

0.1.1 was tested end to end on 2026-10-18 against a dev Code Foundry (73685d8b) in an
isolated `CODE_FOUNDRY_HOME`: two threads on code-foundry's PR #196 (`cf/plugins-7-registry`)
share one `pr:alexwaumann/code-foundry#196` panel, opened by the first; it showed `ci #508`
passed and selected, its jobs (1 passed, 3 skipped) and "Nothing failed in this run".
`send-failure` from the second thread said "no failed run for …#196 at 73685d8"; with
`--run 39130450084` (a failed `ci #498`) it read the run, its jobs and the failed log from
GitHub and sent the excerpt to the second thread, not the panel's opener.

0.1.0 was tested end to end on 2026-10-18 against a dev Code Foundry (8ab4f619) in an isolated
`CODE_FOUNDRY_HOME`: a thread on code-foundry's `cf/plugins-4-skill` branch (PR #197,
run `ci #496`: `attested` passed, three jobs skipped) showed the run, its jobs, "Nothing
failed in this run" on Log, and `open` opened the run. A draft pull request here with a
deliberately failing test (#1, closed) showed the failed run, `test` failed at `Test`,
the toned `--- FAIL: TestDeliberateFailure` tail, flagged the thread "GitHub Actions:
test failed", `rerun-failed` started attempt 2 (flagged as a new failure, not repeated
after a plugin restart), and pushing a passing commit cleared the flag.

Releases: bump `version` in `plugin.json`, tag `v<version>` and push the tag; the release
workflow tests, builds the darwin/arm64 and darwin/amd64 tarballs (`make dist`) and
attaches them with `SHA256SUMS`. Then `make registry-entry` writes `dist/gh-actions.json`
for the registry pull request.

## Changelog

### 0.2.0

- Wake source `run-finished` (args `pr`, from the thread's pull request, and optional
  `run`; scope pr): fires when a workflow run on the pull request's head completes, once
  per run attempt, with the run, its conclusion and failed jobs in the payload. A
  subscription re-arms after a restart (the host sends it again); the pull request is
  polled only while it has one.
- *Send failure to Claude*'s message, the `rerun-failed`, `rerun` and `cancel` results
  and a new notice in the panel under a failed or running run end with the `code-foundry
  thread subscribe --plugin gh-actions/run-finished …` line (not the 80-character flag
  reason).
- Built against pluginsdk v0.2.0 (code-foundry `363eda43`). Same permissions as 0.1.2
  (a wake source needs none), so updating keeps it enabled.

### 0.1.2

- Built against the released Code Foundry SDK, `pluginsdk` v0.1.0 (code-foundry
  `4ed617fd`): the surface's `Close` takes the close reason the host now sends. No
  behaviour change; same permissions as 0.1.1, so updating keeps it enabled.
- Tests: the harness shuts the plugin down before its data dir is removed (`TestFlagSweep`
  could fail under `-race` when the flag watcher wrote `flags.json` during cleanup).

### 0.1.1

- The run selected at first is the newest that failed or is still going, else the
  newest; runs created in the same second order by run id, so a passing run is no longer
  hidden behind the run its concurrency group cancelled (code-foundry `ci #506`/`#507`).
- *Send failure to Claude* is a command (`send-failure`, also in the palette and CLI): the
  app runs the view's button with the thread you are looking at. Before, it went to the
  thread that first opened the shared pull request panel. An event from an older view
  uses the acting panel's thread when the app says which.
- Tab keys `tab-runs`, `tab-jobs`, `tab-log` (`plugin act … tab --key tab-jobs`); each run
  attempt and job selection gets its own log node, so a rerun's log never continues the
  last attempt's lines.
- Same permissions as 0.1.0: updating keeps it enabled.
- Built against the Code Foundry SDK at 73685d8b; `make registry-entry` writes the registry
  entry.

### 0.1.0

- First release.

## License

Apache 2.0
