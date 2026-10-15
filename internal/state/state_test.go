package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltOnlyMatchesTheExactSHA(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if s.Built("main", "abc123") {
		t.Error("nothing has been built yet")
	}
	if err := s.Record("main", "abc123"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !s.Built("main", "abc123") {
		t.Error("the recorded SHA should count as built")
	}
	// The point of the check: a new commit on a built ref is new work.
	if s.Built("main", "def456") {
		t.Error("a different SHA on the same ref must still be built")
	}
}

func TestRecordSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Record("pr/7", "cafe123"); err != nil {
		t.Fatalf("record: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.Built("pr/7", "cafe123") {
		t.Error("a restart must not re-run a build that already happened")
	}
}

func TestOpenToleratesACorruptFile(t *testing.T) {
	// Losing the file costs a rebuild of commits that were already green.
	// Refusing to start costs every build until someone notices.
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a corrupt state file must not stop the service: %v", err)
	}
	if s.Built("main", "abc123") {
		t.Error("a corrupt file should read as empty, not as arbitrary state")
	}
	if err := s.Record("main", "abc123"); err != nil {
		t.Fatalf("and it must still be writable afterwards: %v", err)
	}
}

func TestForgetDropsClosedPullRequestsButKeepsMain(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for ref, sha := range map[string]string{"main": "aaa", "pr/1": "bbb", "pr/2": "ccc"} {
		if err := s.Record(ref, sha); err != nil {
			t.Fatalf("record %s: %v", ref, err)
		}
	}

	s.Forget(map[string]bool{"pr/2": true}) // only pr/2 is still open

	if !s.Built("main", "aaa") {
		t.Error("main is never forgotten — it is not a pull request")
	}
	if !s.Built("pr/2", "ccc") {
		t.Error("an open pull request must be kept")
	}
	if s.Built("pr/1", "bbb") {
		t.Error("a closed pull request should not accumulate for ever")
	}
}

func TestForgetKeepsAnyBranchRefWhateverItIsCalled(t *testing.T) {
	// Forget used to spare the literal "main", so a runner configured with
	// CI_BRANCH=develop would drop its own branch from state on every pass
	// and rebuild it for ever.
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for ref, sha := range map[string]string{"develop": "aaa", "release/2.0": "bbb", "pr/9": "ccc"} {
		if err := s.Record(ref, sha); err != nil {
			t.Fatalf("record %s: %v", ref, err)
		}
	}

	s.Forget(map[string]bool{}) // no pull requests are open

	if !s.Built("develop", "aaa") || !s.Built("release/2.0", "bbb") {
		t.Error("branch refs must survive, whatever they are named")
	}
	if s.Built("pr/9", "ccc") {
		t.Error("a closed pull request should be dropped")
	}
}

func TestRecordWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Record("main", "abc"); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The temporary file is renamed over the target, never left behind:
	// a half-written state file read at the next start would lose every ref.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}
