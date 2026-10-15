// Package state remembers which commits have already been built, so a
// restart does not re-run the suite for everything it can see.
//
// A JSON file rather than a database: the whole state is a map of ref to
// SHA, it is written once per build, and a service that needs a database
// to remember one string per branch is a service with a dependency it has
// not earned.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	path string
	mu   sync.Mutex
	Seen map[string]string `json:"seen"` // ref -> last built SHA
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, Seen: map[string]string{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	// A corrupt state file must not stop the service: the cost is rebuilding
	// commits that were already green, which is cheap and self-correcting.
	if err := json.Unmarshal(b, s); err != nil || s.Seen == nil {
		s.Seen = map[string]string{}
	}
	return s, nil
}

// Built reports whether this exact SHA was the last one built for a ref.
func (s *Store) Built(ref, sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Seen[ref] == sha
}

// Record marks a SHA as built and writes the file through immediately —
// the next build is minutes away, so there is nothing to batch, and a
// crash between build and write would repeat a full suite run.
func (s *Store) Record(ref, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Seen[ref] = sha

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temporary file and rename: a half-written state file read
	// at the next start would lose every ref, not just the one in flight.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Forget drops refs that no longer exist, so closed pull requests do not
// accumulate in the file for ever.
func (s *Store) Forget(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ref := range s.Seen {
		if ref != "main" && !keep[ref] {
			delete(s.Seen, ref)
		}
	}
}
