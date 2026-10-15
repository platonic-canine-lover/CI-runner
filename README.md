# ci-runner

A continuous-integration worker: it watches a GitHub repository, runs your
build and test steps on a machine you control, and reports the result back
as a commit status.

No dependencies outside the standard library.

```
$ CI_REPO=owner/name CI_STEPS=./steps.json ci-runner
2026/10/15 13:34 ci-runner: owner/name as you, polling every 2m0s, logs in ~/.ci-runner/logs
2026/10/15 13:35 building main 06f7fb2 — a commit message
2026/10/15 13:40 main 06f7fb2: passed in 4m57s (~/.ci-runner/logs/20261015-133500-06f7fb2.log)
```

## Why

Hosted CI stops for reasons that have nothing to do with your code — a
lapsed payment, an exhausted quota, a provider outage. The repository goes
on looking the same while nothing is being verified, which is worse than
having no CI at all, because the badge still says what it always said.

This runs on hardware you already own, costs nothing per minute, and keeps
working when the hosted account does not.

## What it does not do

It does **not** execute workflow files. A self-hosted GitHub Actions runner
will run whatever YAML a branch contains; if that machine also runs
something you care about, that is a wide door into it. This runs only the
steps named in its own configuration, against a checkout in its own
directory.

It does not receive webhooks either, at least not yet. It polls. Polling
needs no inbound network exposure at all, which suits a machine behind a
domestic router. The HTTP server it does run binds to loopback and only
reads.

Less capable on purpose, in both cases.

## Security

This runs build steps on your machine, so it is worth being explicit about
what that means.

**Pull requests from forks are not built.** A build step runs whatever the
repository says — `npm test` runs the scripts in that branch's
`package.json` — so building a stranger's branch is running their code on
your hardware. Forks are skipped and reported on the commit as an error
explaining why, rather than silently ignored.

**Build steps do not inherit the service's environment.** They get `PATH`,
`HOME`, `LANG`, `LC_ALL`, `TMPDIR`, `SHELL`, `USER` and `TZ`, plus whatever
the step declares — and nothing else. The GitHub token the service uses to
report status never reaches a build.

**Steps are what you configured, not what a branch asks for.** There is no
workflow file in the repository that this obeys. Changing what runs means
changing the steps file on the runner's own machine.

**It is still your machine.** Your own code runs with your user's
permissions and can reach anything that user can. On a repository where
you trust every contributor with push access, that is the same trust you
have already extended. If that is not true, run it as a dedicated user.

## Design notes

**Builds are serial.** Not because concurrency is hard, but because test
suites routinely assume they are alone — shared fixtures, a database named
after the test file, a fixed port. The queue has no concurrency to
misconfigure.

**Every step is bounded.** One deadline covers the whole build and each
command inherits it, so a hung step fails the build instead of holding the
runner for ever.

**Steps are argv, not a command string.** `["npm", "test"]` rather than
`npm test`, so nothing is word-split or shell-expanded and a path with a
space in it behaves.

**A red build is a result, not an error.** The runner returns an error only
when the runner itself is broken; GitHub hears "failure" for a failing
suite and "error" for a runner that could not do its job. Those are
different problems and deserve different signals.

**Red builds are recorded too**, so a failing commit is not rebuilt every
two minutes until somebody pushes a fix.

**The state file is a JSON map of ref to SHA**, written through a temporary
file and renamed. A service that needs a database to remember one string
per branch has a dependency it has not earned. A corrupt state file reads
as empty rather than stopping the service: the cost is rebuilding commits
that were already green, which is cheap and self-correcting.

## Configuration

Environment variables, all optional except the repository:

| Variable | Default | Meaning |
|---|---|---|
| `CI_REPO` | — | `owner/name`, required |
| `CI_GITHUB_TOKEN` | `gh auth token` | token with `repo` scope |
| `CI_STEPS` | a default Node build | path to a steps file |
| `CI_BRANCH` | `main` | branch to watch |
| `CI_INTERVAL` | `2m` | how often to look for new commits |
| `CI_TIMEOUT` | `25m` | budget for one whole build |
| `CI_WORKDIR` | `~/.ci-runner/checkout` | where the repository is checked out |
| `CI_LOGDIR` | `~/.ci-runner/logs` | one log file per build |
| `CI_STATE` | `~/.ci-runner/state.json` | what has been built |
| `CI_LISTEN` | `127.0.0.1:3101` | status endpoint |

If `CI_GITHUB_TOKEN` is unset it falls back to the `gh` CLI's token, which
is usually already on the machine with the right scopes — one less secret
to copy into a service definition.

### Steps

```json
[
  { "name": "install", "args": ["npm", "ci", "--no-audit"] },
  { "name": "lint",    "args": ["npm", "run", "lint"] },
  { "name": "test",    "args": ["npm", "test"], "env": ["TEST_DB=/tmp/test.db"] }
]
```

`env` is how a step gets anything beyond the minimal environment above —
a database path, a feature flag, a fixture directory.

Steps run in order in the checkout directory and stop at the first failure.
JSON rather than YAML so the binary keeps no dependencies.

### Steps have to be idempotent

This is the one thing that catches people moving from hosted CI, and it
does not show up until the second run.

A hosted runner hands you a clean machine every time, so a step can assume
nothing exists yet. This runner reuses its checkout — deliberately, because
re-cloning and reinstalling dependencies for every build is most of the
wall-clock time — and the machine's `/tmp` persists too. A step that
creates something therefore has to tolerate that thing already being
there.

The first real build here failed on exactly that:

```
ABORT: /tmp/ci-runner-warburton.db already exists. Use --force to overwrite (destructive).
```

The project's database initialiser refuses to overwrite, which is right on
a developer's machine and wrong in a build. The fix is in the step, not the
project:

```json
{ "name": "db-init", "args": ["node", "db/init.js", "--force"],
  "env": ["TEST_DB=/tmp/test.db"] }
```

The same applies to anything else that survives a build: caches, generated
files, a port left bound. Write the steps as though the last run already
happened, because it did.

## Endpoints

- `GET /healthz` — `ok`
- `GET /status` — what it is doing and how the last build went

```json
{
  "repo": "owner/name",
  "busy": false,
  "last_build": {
    "ref": "main", "sha": "06f7fb2", "ok": true,
    "at": "2026-10-15T13:40:02Z", "duration": "4m57s",
    "log": "/Users/you/.ci-runner/logs/20261015-133500-06f7fb2.log"
  }
}
```

## Scheduling it

The daemon polls, which is what you want if a commit should be checked
minutes after it lands. For "run the suite at 01:10 whatever happened
today", use one pass and exit:

```sh
ci-runner -once
```

That suits cron, a systemd timer, or a launchd agent — see
[`docs/launchd.md`](docs/launchd.md).

**Exit status says whether the RUNNER worked, not whether the build
passed.** It exits non-zero when GitHub could not be reached, the branch
was not found, or the runner itself broke — things that need your
attention because nothing was verified. A red build exits 0: that result
was reported to the commit, which is where you would look for it. So a
scheduler alerting on non-zero exits tells you "CI did not run" and stays
quiet for "CI ran and the tests failed".

## Running it

```sh
go build -o ci-runner ./cmd/ci-runner
CI_REPO=owner/name ./ci-runner
```

On macOS, as a launchd agent — see [`docs/launchd.md`](docs/launchd.md).

## Tests

```sh
go test ./...
```

The runner's tests build a real git repository in a temporary directory and
run real commands against it, rather than stubbing out git. The GitHub
client is tested over `httptest`, including the cases that matter when
something is wrong: a 401 should surface "Bad credentials" rather than just
a status code.

## Licence

MIT.
