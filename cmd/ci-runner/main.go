// ci-runner runs a repository's test suite on a machine you control and
// reports the result to GitHub as a commit status.
//
// Why it exists: hosted CI can stop for reasons that have nothing to do
// with your code — a lapsed payment, an exhausted quota, an outage — and
// the repository goes on looking the same while nothing is actually being
// verified. A runner on hardware you already own does not share that
// failure mode, and costs nothing per minute.
//
// Why not a self-hosted GitHub Actions runner: that executes whatever
// workflow YAML a branch contains. If the machine also runs anything you
// care about, that is a wide door. This runs only the steps compiled into
// it, against a checkout in its own directory. Less capable on purpose.
//
// It polls rather than receiving webhooks. Polling needs no inbound
// network exposure at all, which suits a machine behind a domestic router
// or a host whose services deliberately bind to loopback. The HTTP server
// here is loopback-only too, and only reads: it exists so the service can
// be asked how it is getting on.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/platonic-canine-lover/CI-runner/internal/forge"
	"github.com/platonic-canine-lover/CI-runner/internal/runner"
	"github.com/platonic-canine-lover/CI-runner/internal/state"
)

const checkName = "ci-runner"

type config struct {
	Repo     string
	Token    string
	WorkDir  string
	LogDir   string
	StateF   string
	StepsF   string
	Branch   string
	Interval time.Duration
	Timeout  time.Duration
	Listen   string
}

func loadConfig() (config, error) {
	home, _ := os.UserHomeDir()
	c := config{
		Repo:     env("CI_REPO", ""),
		Token:    os.Getenv("CI_GITHUB_TOKEN"),
		WorkDir:  env("CI_WORKDIR", filepath.Join(home, ".ci-runner", "checkout")),
		LogDir:   env("CI_LOGDIR", filepath.Join(home, ".ci-runner", "logs")),
		StateF:   env("CI_STATE", filepath.Join(home, ".ci-runner", "state.json")),
		StepsF:   os.Getenv("CI_STEPS"),
		Branch:   env("CI_BRANCH", "main"),
		Interval: envDuration("CI_INTERVAL", 2*time.Minute),
		Timeout:  envDuration("CI_TIMEOUT", 25*time.Minute),
		Listen:   env("CI_LISTEN", "127.0.0.1:3101"),
	}
	if c.Token == "" {
		// Fall back to the gh CLI's token, which is already on this machine
		// with the right scopes — one less secret to copy into a plist.
		if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
			c.Token = strings.TrimSpace(string(out))
		}
	}
	if c.Token == "" {
		return c, fmt.Errorf("no token: set CI_GITHUB_TOKEN, or log in with the gh CLI")
	}
	if c.Repo == "" {
		return c, fmt.Errorf("set CI_REPO to the repository to build, as owner/name")
	}
	return c, nil
}

// defaultSteps is what an unconfigured Node project almost certainly
// wants. Anything else comes from a steps file — see loadSteps.
func defaultSteps() []runner.Step {
	return []runner.Step{
		{Name: "install", Args: []string{"npm", "ci", "--no-audit", "--no-fund"}},
		{Name: "test", Args: []string{"npm", "test"}},
	}
}

// loadSteps reads a JSON steps file:
//
//	[
//	  {"name": "install", "args": ["npm", "ci"]},
//	  {"name": "test",    "args": ["npm", "test"], "env": ["TEST_DB=/tmp/t.db"]}
//	]
//
// JSON rather than YAML so the binary keeps no dependencies, and argv
// rather than a command string so nothing is word-split or shell-expanded:
// a step cannot accidentally become two, and a path with a space in it
// behaves.
func loadSteps(path string) ([]runner.Step, error) {
	if path == "" {
		return defaultSteps(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("steps file: %w", err)
	}
	var steps []runner.Step
	if err := json.Unmarshal(b, &steps); err != nil {
		return nil, fmt.Errorf("steps file %s: %w", path, err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("steps file %s defines no steps", path)
	}
	for i, st := range steps {
		if st.Name == "" || len(st.Args) == 0 {
			return nil, fmt.Errorf("steps file %s: step %d needs a name and args", path, i)
		}
	}
	return steps, nil
}

type service struct {
	cfg   config
	gh    *forge.Client
	run   *runner.Runner
	store *state.Store
	mu    sync.Mutex
	last  buildInfo
	busy  bool
}

type buildInfo struct {
	Ref      string    `json:"ref"`
	SHA      string    `json:"sha"`
	OK       bool      `json:"ok"`
	Failed   string    `json:"failed_step,omitempty"`
	At       time.Time `json:"at"`
	Duration string    `json:"duration"`
	LogPath  string    `json:"log"`
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	// One pass and exit, for a scheduled run (cron, launchd, a CI step in
	// someone else's pipeline). The daemon is the default because it is the
	// useful mode when you want a commit checked minutes after it lands;
	// -once is the useful mode when you want the suite run at 01:00
	// whatever happened during the day.
	once := flag.Bool("once", false, "run one pass over the repository and exit")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	gh := forge.New(cfg.Token, cfg.Repo)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Fail at boot on a bad token rather than on the first status write.
	who, err := gh.Whoami(ctx)
	if err != nil {
		log.Fatalf("github: %v", err)
	}

	store, err := state.Open(cfg.StateF)
	if err != nil {
		log.Fatalf("state: %v", err)
	}

	// Create the service's own directory up front: a steps file that puts a
	// database or a fixture under it should not fail on a clean machine for
	// want of a parent directory, which is how the first real run failed.
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".ci-runner"), 0o755); err != nil {
		log.Fatalf("work dir: %v", err)
	}

	buildSteps, err := loadSteps(cfg.StepsF)
	if err != nil {
		log.Fatalf("steps: %v", err)
	}

	svc := &service{
		cfg:   cfg,
		gh:    gh,
		store: store,
		run: &runner.Runner{
			RepoURL: "https://github.com/" + cfg.Repo + ".git",
			WorkDir: cfg.WorkDir,
			LogDir:  cfg.LogDir,
			Steps:   buildSteps,
			Timeout: cfg.Timeout,
		},
	}

	if *once {
		log.Printf("ci-runner: %s as %s, one pass, logs in %s", cfg.Repo, who, cfg.LogDir)
		if err := svc.pass(ctx); err != nil {
			// Exit non-zero so a scheduler can tell "CI did not run" from
			// "CI ran and the build was red" — the first needs attention,
			// the second is already on the commit.
			log.Printf("ci-runner: pass failed: %v", err)
			os.Exit(1)
		}
		log.Printf("ci-runner: done")
		return
	}

	log.Printf("ci-runner: %s as %s, polling every %s, logs in %s",
		cfg.Repo, who, cfg.Interval, cfg.LogDir)

	go svc.serve(ctx)
	svc.loop(ctx)
	log.Printf("ci-runner: stopped")
}

// loop polls for work. One pass at a time, and one build at a time inside
// a pass: the suite uses per-file test databases named after the file, so
// two concurrent runs corrupt each other's results.
func (s *service) loop(ctx context.Context) {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	if err := s.pass(ctx); err != nil { // don't wait a full interval for the first build
		log.Printf("pass: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.pass(ctx); err != nil {
				log.Printf("pass: %v", err) // the daemon keeps polling
			}
		}
	}
}

// pass returns an error when the RUNNER could not do its job — GitHub
// unreachable, the branch not found. A red build is not an error here: it
// was reported to GitHub, which is where it belongs. The distinction is
// what makes -once's exit status meaningful to cron or launchd.
func (s *service) pass(ctx context.Context) error {
	var work []forge.Commit
	var failure error

	if head, err := s.gh.BranchHead(ctx, s.cfg.Branch); err != nil {
		log.Printf("%s: %v", s.cfg.Branch, err)
		failure = fmt.Errorf("branch %s: %w", s.cfg.Branch, err)
	} else {
		work = append(work, head)
	}

	prs, err := s.gh.OpenPullRequests(ctx)
	if err != nil {
		// One API blip must not wipe what has been built: Forget below runs
		// only when the listing succeeded, or an empty result would drop
		// every pull request from state and rebuild them all next pass.
		log.Printf("pulls: %v", err)
		if failure == nil {
			failure = fmt.Errorf("listing pull requests: %w", err)
		}
	} else {
		keep := map[string]bool{}
		for _, pr := range prs {
			keep[pr.Ref] = true
		}
		s.store.Forget(keep)
	}

	for _, pr := range prs {
		// A pull request from a fork is somebody else's code, and a build
		// step runs what the repository tells it to. On a public repository
		// that is arbitrary code execution on this machine, which is the
		// door the README claims is shut. Report it rather than silently
		// ignoring it, so a contributor is not left waiting for a check.
		if pr.Fork {
			log.Printf("skipping %s — from a fork", pr.Ref)
			if !s.store.Built(pr.Ref, pr.SHA) {
				if err := s.gh.SetStatus(ctx, pr.SHA, forge.StateError, checkName,
					"not built: pull requests from forks are not run on this runner"); err != nil {
					log.Printf("status: %v", err)
				}
				if err := s.store.Record(pr.Ref, pr.SHA); err != nil {
					log.Printf("state: %v", err)
				}
			}
			continue
		}
		work = append(work, pr)
	}

	for _, c := range work {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.store.Built(c.Ref, c.SHA) {
			continue
		}
		if err := s.build(ctx, c); err != nil && failure == nil {
			failure = err
		}
	}
	return failure
}

func (s *service) build(ctx context.Context, c forge.Commit) error {
	s.mu.Lock()
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	log.Printf("building %s %s — %s", c.Ref, short(c.SHA), c.Title)
	if err := s.gh.SetStatus(ctx, c.SHA, forge.StatePending, checkName, "running on the self-hosted runner"); err != nil {
		log.Printf("status pending: %v", err)
	}

	res, err := s.run.Run(ctx, c.SHA)
	if err != nil {
		// The runner itself broke — not the build. Say so on the commit
		// as "error" rather than "failure", which means a red test suite.
		log.Printf("runner: %v", err)
		_ = s.gh.SetStatus(ctx, c.SHA, forge.StateError, checkName, "runner error: "+err.Error())
		return fmt.Errorf("build %s: %w", short(c.SHA), err)
	}

	state, desc := forge.StateSuccess, fmt.Sprintf("passed in %s", res.Duration.Round(time.Second))
	if !res.OK {
		state = forge.StateFailure
		desc = fmt.Sprintf("%s failed after %s", res.Failed, res.Duration.Round(time.Second))
	}
	if err := s.gh.SetStatus(ctx, c.SHA, state, checkName, desc); err != nil {
		log.Printf("status: %v", err)
	}
	log.Printf("%s %s: %s (%s)", c.Ref, short(c.SHA), desc, res.LogPath)

	// Recorded whatever the outcome — a red commit should not be rebuilt
	// every two minutes until someone pushes a fix — EXCEPT when the run was
	// interrupted. A shutdown mid-build (launchd, the nightly restart) kills
	// the step and the status write fails because the context is already
	// cancelled; recording it then would leave GitHub showing "pending" for
	// ever, with the commit marked built and never retried.
	if ctx.Err() != nil {
		log.Printf("%s %s: interrupted — not recorded, it will be retried", c.Ref, short(c.SHA))
		return ctx.Err()
	}
	if err := s.store.Record(c.Ref, c.SHA); err != nil {
		log.Printf("state: %v", err)
	}
	s.mu.Lock()
	s.last = buildInfo{
		Ref: c.Ref, SHA: c.SHA, OK: res.OK, Failed: res.Failed,
		At: time.Now(), Duration: res.Duration.Round(time.Second).String(), LogPath: res.LogPath,
	}
	s.mu.Unlock()
	return nil
}

// serve exposes a loopback status endpoint. Nothing here is a control
// surface: it only reads, and it binds to localhost, so the service can be
// asked how it is getting on without being exposed to anything.
func (s *service) serve(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		out := struct {
			Repo string    `json:"repo"`
			Busy bool      `json:"busy"`
			Last buildInfo `json:"last_build"`
		}{s.cfg.Repo, s.busy, s.last}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	srv := &http.Server{Addr: s.cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("http: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
