# code-foundry-plugin-gh-actions

A [Code Foundry](https://github.com/alexwaumann/code-foundry) plugin that tracks the
GitHub Actions runs of a thread's pull request: what ran on its head commit, which job
failed at which step, the failed step's log, and the buttons to rerun, cancel, or hand
the failure to Claude. It flags the thread when a run fails.

```
code-foundry plugin install gh-actions
```

(Until `plugin install` ships in Code Foundry, unpack a release tarball or clone this
repository and run `code-foundry plugin dev <folder> --yes`; see Development.)

## What it shows

An **Actions** tab in the side panel of any thread whose pull request has GitHub Actions
checks (one panel per pull request, shared by the threads on it):

- **Header**: the pull request's number and title, the selected run's workflow, a status
  pill (passed, failed, running, queued, cancelled, …), how long it took or a live
  elapsed time, the head commit, and buttons to open the run on GitHub and refresh.
- **Runs**: every workflow run of the pull request's head commit with its status, run
  number and attempt, event, and duration. Click one to see its jobs.
- **Jobs**: the selected run's jobs with status and duration; a failed job names the step
  that failed. Click one to see its log.
- **Log**: the failed steps' log tail (the last 200 lines of each failed job, timestamps
  and colour codes stripped, error lines red, warnings amber). While a run is still going,
  each job that fails is appended as it finishes.
- **Actions**: *Send failure to Claude* sends the panel's thread a request to investigate,
  with the workflow, job, step, run URL and up to 60 log lines (the error lines and the
  last lines, at most 4 KB). *Rerun failed* and *Rerun all* for a finished run, *Cancel*
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

**Commands** (palette under *GitHub Actions*, CLI `code-foundry plugin gh-actions …`),
each for the active thread or `--thread <id>`, optionally `--run <id>` (or the run id as
a word):

| Command | Does |
|---|---|
| `rerun-failed` | Rerun the failed jobs of the newest failed (or cancelled) run |
| `rerun` | Rerun every job of the newest finished run |
| `cancel` | Cancel the run in progress (asks first; `--yes` on the CLI) |
| `open` | Open the run on GitHub: the one in progress, else the failed one, else the newest |

## Permissions and why

| Permission | Why |
|---|---|
| commands `pr.checks` | Find the thread's pull request and its checks (the flag, and the panel's pull request when it is opened from the palette) |
| commands `thread.send` | *Send failure to Claude* |
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
code-foundry plugin act gh-actions runs tab --key jobs --thread <thread>
code-foundry plugin act gh-actions runs select-run --thread <thread>
code-foundry plugin gh-actions open --thread <thread>
code-foundry plugin logs gh-actions --lines 50
make build && code-foundry plugin restart gh-actions   # after every change
make test
```

Tested end to end on 2026-10-18 against a dev Code Foundry (8ab4f619) in an isolated
`CODE_FOUNDRY_HOME`: a thread on code-foundry's `cf/plugins-4-skill` branch (PR #197,
run `ci #496`: `attested` passed, three jobs skipped) showed the run, its jobs, "Nothing
failed in this run" on Log, and `open` opened the run. A draft pull request here with a
deliberately failing test (#1, closed) showed the failed run, `test` failed at `Test`,
the toned `--- FAIL: TestDeliberateFailure` tail, flagged the thread "GitHub Actions:
test failed", `rerun-failed` started attempt 2 (flagged as a new failure, not repeated
after a plugin restart), and pushing a passing commit cleared the flag.

Releases: bump `version` in `plugin.json`, tag `v<version>` and push the tag; the release
workflow tests, builds the darwin/arm64 and darwin/amd64 tarballs (`make dist`) and
attaches them with `SHA256SUMS`.

## License

Apache 2.0
