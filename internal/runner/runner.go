// Package runner checks a commit out and runs the build steps against it.
//
// Two properties matter more than speed here:
//
//   - One job at a time. The suite this was written for uses per-file test
//     databases named after the file, so two concurrent runs clobber each
//     other and fail in ways that look like unrelated bugs. The queue is
//     serial by construction, not by convention.
//   - Every step is bounded. A hung step must fail the build rather than
//     hold the runner for ever; the context deadline covers the whole job
//     and each command inherits it.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Step is one named command in a build.
type Step struct {
	Name string   `json:"name"`
	Args []string `json:"args"` // argv, run directly — no shell, so nothing is word-split
	Env  []string `json:"env"`  // extra environment, "KEY=value"
}

// Result is what happened to a build.
type Result struct {
	OK       bool
	Failed   string // the step that failed, empty when OK
	Output   string // trailing output, for the status description and the log
	Duration time.Duration
	LogPath  string
}

// Runner clones into WorkDir and runs Steps there.
type Runner struct {
	RepoURL string
	WorkDir string // checkout lives here, reused between builds
	LogDir  string // one log file per build
	Steps   []Step
	Timeout time.Duration // whole-build budget
}

// Run checks out sha and runs every step in order, stopping at the first
// failure. It never returns an error for a failing build — a red build is
// a Result, not a malfunction. An error means the runner itself broke.
func (r *Runner) Run(ctx context.Context, sha string) (Result, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("log dir: %w", err)
	}
	logPath := filepath.Join(r.LogDir, fmt.Sprintf("%s-%s.log", time.Now().Format("20060102-150405"), short(sha)))
	logFile, err := os.Create(logPath)
	if err != nil {
		return Result{}, fmt.Errorf("log file: %w", err)
	}
	defer logFile.Close()

	res := Result{LogPath: logPath}
	if err := r.checkout(ctx, sha, logFile); err != nil {
		res.Duration = time.Since(started)
		res.Failed = "checkout"
		res.Output = err.Error()
		fmt.Fprintf(logFile, "\n=== checkout failed: %v\n", err)
		return res, nil
	}

	for _, step := range r.Steps {
		fmt.Fprintf(logFile, "\n=== %s: %s\n", step.Name, strings.Join(step.Args, " "))
		out, err := r.exec(ctx, step, logFile)
		if err != nil {
			res.Duration = time.Since(started)
			res.Failed = step.Name
			res.Output = tail(out, 400)
			fmt.Fprintf(logFile, "\n=== FAILED at %s after %s: %v\n", step.Name, res.Duration.Round(time.Second), err)
			return res, nil
		}
	}

	res.OK = true
	res.Duration = time.Since(started)
	fmt.Fprintf(logFile, "\n=== passed in %s\n", res.Duration.Round(time.Second))
	return res, nil
}

// checkout fetches the exact sha into a reused working copy. Fetching one
// commit rather than cloning afresh keeps a full npm install off the
// critical path for every build; --force and a hard reset mean a previous
// build's mess cannot leak into this one.
func (r *Runner) checkout(ctx context.Context, sha string, log *os.File) error {
	if _, err := os.Stat(filepath.Join(r.WorkDir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(r.WorkDir), 0o755); err != nil {
			return err
		}
		if out, err := r.git(ctx, filepath.Dir(r.WorkDir), log, "clone", "--quiet", r.RepoURL, r.WorkDir); err != nil {
			return fmt.Errorf("clone: %s", tail(out, 200))
		}
	}
	if out, err := r.git(ctx, r.WorkDir, log, "fetch", "--quiet", "--force", "origin", sha); err != nil {
		return fmt.Errorf("fetch %s: %s", short(sha), tail(out, 200))
	}
	if out, err := r.git(ctx, r.WorkDir, log, "reset", "--hard", "--quiet", sha); err != nil {
		return fmt.Errorf("reset %s: %s", short(sha), tail(out, 200))
	}
	// Untracked leftovers (node_modules is ignored and deliberately kept;
	// anything else a build wrote is not wanted by the next one).
	if out, err := r.git(ctx, r.WorkDir, log, "clean", "-qfd", "-e", "node_modules"); err != nil {
		return fmt.Errorf("clean: %s", tail(out, 200))
	}
	return nil
}

func (r *Runner) git(ctx context.Context, dir string, log *os.File, args ...string) (string, error) {
	return r.exec(ctx, Step{Name: "git", Args: append([]string{"git"}, args...)}, log, dir)
}

func (r *Runner) exec(ctx context.Context, step Step, log *os.File, dirOverride ...string) (string, error) {
	dir := r.WorkDir
	if len(dirOverride) > 0 {
		dir = dirOverride[0]
	}
	cmd := exec.CommandContext(ctx, step.Args[0], step.Args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), step.Env...)

	// Tee to both: the buffer feeds the status description, the file is
	// the record someone reads afterwards.
	var buf strings.Builder
	both := io.MultiWriter(&buf, log)
	cmd.Stdout = both
	cmd.Stderr = both
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return buf.String(), fmt.Errorf("timed out after %s", r.Timeout)
	}
	return buf.String(), err
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
