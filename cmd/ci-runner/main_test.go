package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSteps(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "steps.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoadStepsFallsBackToADefaultNodeBuild(t *testing.T) {
	steps, err := loadSteps("")
	if err != nil {
		t.Fatalf("loadSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("an unconfigured runner should still do something useful")
	}
}

func TestLoadStepsReadsNameArgsAndEnv(t *testing.T) {
	path := writeSteps(t, `[
	  {"name": "install", "args": ["npm", "ci"]},
	  {"name": "test", "args": ["npm", "test"], "env": ["TEST_DB=/tmp/t.db"]}
	]`)

	steps, err := loadSteps(path)
	if err != nil {
		t.Fatalf("loadSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].Name != "install" || steps[0].Args[1] != "ci" {
		t.Errorf("first step parsed wrong: %+v", steps[0])
	}
	if len(steps[1].Env) != 1 || steps[1].Env[0] != "TEST_DB=/tmp/t.db" {
		t.Errorf("env not parsed: %+v", steps[1])
	}
}

func TestLoadStepsRejectsFilesThatWouldSilentlyDoNothing(t *testing.T) {
	cases := map[string]string{
		"empty list": `[]`,
		"no args":    `[{"name": "test"}]`,
		"no name":    `[{"args": ["npm", "test"]}]`,
		"not json":   `npm test`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSteps(writeSteps(t, body)); err == nil {
				t.Error("expected an error rather than a runner that builds nothing")
			}
		})
	}
}

func TestLoadStepsReportsAMissingFile(t *testing.T) {
	_, err := loadSteps(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("expected an error")
	}
	// Misconfiguration should name the file, not just the syscall.
	if !strings.Contains(err.Error(), "steps file") {
		t.Errorf("unhelpful error: %v", err)
	}
}
