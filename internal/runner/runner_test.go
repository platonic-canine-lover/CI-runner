package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRepo makes a real git repository with one commit, so the runner can be
// exercised against git rather than against a stub of it.
func newRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "first")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return dir, strings.TrimSpace(string(out))
}

func newRunner(t *testing.T, repo string, steps []Step) *Runner {
	t.Helper()
	base := t.TempDir()
	return &Runner{
		RepoURL: repo,
		WorkDir: filepath.Join(base, "checkout"),
		LogDir:  filepath.Join(base, "logs"),
		Steps:   steps,
		Timeout: 30 * time.Second,
	}
}

func TestRunPassesAndLogs(t *testing.T) {
	repo, sha := newRepo(t)
	r := newRunner(t, repo, []Step{
		{Name: "read-marker", Args: []string{"cat", "marker.txt"}},
	})

	res, err := r.Run(context.Background(), sha)
	if err != nil {
		t.Fatalf("runner broke: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected a pass, got failure at %q: %s", res.Failed, res.Output)
	}

	log, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatalf("no build log written: %v", err)
	}
	if !strings.Contains(string(log), "hello") {
		t.Error("the build log should carry the step's own output")
	}
}

func TestRunStopsAtTheFirstFailingStep(t *testing.T) {
	repo, sha := newRepo(t)
	marker := filepath.Join(t.TempDir(), "should-not-exist")
	r := newRunner(t, repo, []Step{
		{Name: "failing", Args: []string{"false"}},
		{Name: "after", Args: []string{"touch", marker}},
	})

	res, err := r.Run(context.Background(), sha)
	if err != nil {
		t.Fatalf("a failing build is a result, not an error: %v", err)
	}
	if res.OK {
		t.Fatal("expected failure")
	}
	if res.Failed != "failing" {
		t.Errorf("expected the failing step to be named, got %q", res.Failed)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("steps after a failure must not run")
	}
}

func TestStepEnvironmentReachesTheCommand(t *testing.T) {
	repo, sha := newRepo(t)
	out := filepath.Join(t.TempDir(), "env.txt")
	r := newRunner(t, repo, []Step{
		// sh -c is the test's own choice; the runner itself never shells out.
		{Name: "env", Args: []string{"sh", "-c", "printf %s \"$CI_RUNNER_FIXTURE\" > " + out}, Env: []string{"CI_RUNNER_FIXTURE=present"}},
	})

	res, err := r.Run(context.Background(), sha)
	if err != nil || !res.OK {
		t.Fatalf("build failed: %v %+v", err, res)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "present" {
		t.Errorf("step env not passed through, got %q", got)
	}
}

func TestBuildStepsCannotSeeTheServiceEnvironment(t *testing.T) {
	// The service's own environment holds its GitHub token. A build step
	// runs repository code, so it must not inherit it.
	t.Setenv("CI_GITHUB_TOKEN", "a-real-secret")
	t.Setenv("SOME_OTHER_SECRET", "also-private")

	repo, sha := newRepo(t)
	out := filepath.Join(t.TempDir(), "leaked.txt")
	r := newRunner(t, repo, []Step{
		{Name: "dump", Args: []string{"sh", "-c", "env > " + out}},
	})

	if res, err := r.Run(context.Background(), sha); err != nil || !res.OK {
		t.Fatalf("build failed: %v %+v", err, res)
	}
	leaked, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, secret := range []string{"a-real-secret", "also-private"} {
		if strings.Contains(string(leaked), secret) {
			t.Errorf("the step could read %q from the service's environment", secret)
		}
	}
	// But it still needs enough to find a toolchain.
	if !strings.Contains(string(leaked), "PATH=") {
		t.Error("PATH must be passed through or no step can find its tools")
	}
}

func TestRunTimesOutRatherThanHanging(t *testing.T) {
	repo, sha := newRepo(t)
	r := newRunner(t, repo, []Step{{Name: "sleep", Args: []string{"sleep", "30"}}})
	r.Timeout = 300 * time.Millisecond

	start := time.Now()
	res, err := r.Run(context.Background(), sha)
	if err != nil {
		t.Fatalf("a timeout is a failed build, not a runner error: %v", err)
	}
	if res.OK {
		t.Fatal("a step that outlives the budget must fail the build")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s — the deadline did not cut it short", elapsed)
	}
}

func TestDeadlineHoldsWhenAStepLeavesAChildRunning(t *testing.T) {
	// The case the first timeout test missed: `sleep` starts no children, so
	// killing the direct child was enough and the bug stayed hidden. A step
	// that backgrounds something — `npm test` leaving a server up — hands
	// the stdout pipe to a grandchild, and Wait() blocks on that pipe long
	// after the deadline. Measured at 8.1s against a 300ms budget before
	// the process-group kill; 304ms after.
	repo, sha := newRepo(t)
	r := newRunner(t, repo, []Step{{Name: "orphan", Args: []string{"sh", "-c", "sleep 8 & sleep 8"}}})
	r.Timeout = 300 * time.Millisecond

	start := time.Now()
	res, err := r.Run(context.Background(), sha)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a timeout is a failed build, not a runner error: %v", err)
	}
	if res.OK {
		t.Error("a step that outlives its budget must fail the build")
	}
	if elapsed > 2*time.Second {
		t.Errorf("the deadline did not hold: took %s on a 300ms budget", elapsed.Round(time.Millisecond))
	}
}

func TestCheckoutIsCleanBetweenBuilds(t *testing.T) {
	repo, sha := newRepo(t)
	stray := "stray.txt"
	r := newRunner(t, repo, []Step{{Name: "litter", Args: []string{"sh", "-c", "echo mess > " + stray}}})

	if res, err := r.Run(context.Background(), sha); err != nil || !res.OK {
		t.Fatalf("first build: %v %+v", err, res)
	}
	// The second build must not see what the first one left behind.
	r.Steps = []Step{{Name: "check", Args: []string{"sh", "-c", "test ! -f " + stray}}}
	res, err := r.Run(context.Background(), sha)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !res.OK {
		t.Error("a previous build's leftovers leaked into the next checkout")
	}
}

func TestUnknownSHAFailsTheBuildWithoutBreakingTheRunner(t *testing.T) {
	repo, _ := newRepo(t)
	r := newRunner(t, repo, []Step{{Name: "noop", Args: []string{"true"}}})

	res, err := r.Run(context.Background(), "0000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("expected a failed build, not a runner error: %v", err)
	}
	if res.OK || res.Failed != "checkout" {
		t.Errorf("expected a checkout failure, got OK=%v failed=%q", res.OK, res.Failed)
	}
}
