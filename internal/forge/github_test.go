package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withServer points a Client at a test server, so the client is exercised
// over real HTTP rather than against a hand-stubbed interface.
func withServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("test-token", "owner/repo")
	c.base = srv.URL
	return c
}

func TestBranchHeadReadsTheCommit(t *testing.T) {
	c := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("token not sent, got %q", got)
		}
		if r.URL.Path != "/repos/owner/repo/commits/main" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"sha":"abc123","commit":{"message":"a title\n\nand a body"}}`)
	})

	got, err := c.BranchHead(context.Background(), "main")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if got.SHA != "abc123" {
		t.Errorf("sha = %q", got.SHA)
	}
	if got.Title != "a title" {
		t.Errorf("title should be the first line only, got %q", got.Title)
	}
}

func TestOpenPullRequestsAreLabelledByNumber(t *testing.T) {
	c := withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"number":12,"title":"a change","head":{"sha":"deadbee"}}]`)
	})

	prs, err := c.OpenPullRequests(context.Background())
	if err != nil {
		t.Fatalf("OpenPullRequests: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected one pull request, got %d", len(prs))
	}
	if prs[0].Ref != "pr/12" || prs[0].PullNum != 12 || prs[0].SHA != "deadbee" {
		t.Errorf("unexpected pull request: %+v", prs[0])
	}
}

func TestSetStatusTruncatesLongDescriptions(t *testing.T) {
	// GitHub silently shortens descriptions past 140 characters; better to
	// send something we chose than discover a sentence cut mid-word.
	var sent map[string]string
	c := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(http.StatusCreated)
	})

	long := strings.Repeat("x", 500)
	if err := c.SetStatus(context.Background(), "abc", StateFailure, "ci-runner", long); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if len(sent["description"]) > 140 {
		t.Errorf("description not truncated: %d chars", len(sent["description"]))
	}
	if !strings.HasSuffix(sent["description"], "...") {
		t.Error("a truncated description should show that it was cut")
	}
	if sent["state"] != "failure" || sent["context"] != "ci-runner" {
		t.Errorf("unexpected payload: %+v", sent)
	}
}

func TestShortDescriptionsArePassedThrough(t *testing.T) {
	var sent map[string]string
	c := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(http.StatusCreated)
	})

	if err := c.SetStatus(context.Background(), "abc", StateSuccess, "ci-runner", "passed in 5m"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if sent["description"] != "passed in 5m" {
		t.Errorf("description = %q", sent["description"])
	}
}

func TestErrorsCarryTheResponseBody(t *testing.T) {
	// "401 Unauthorized" sends you looking at the network. "Bad credentials"
	// sends you to the token, which is where the problem actually is.
	c := withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	})

	_, err := c.BranchHead(context.Background(), "main")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error should quote the response body, got: %v", err)
	}
}

func TestWhoamiReturnsTheLogin(t *testing.T) {
	c := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"login":"someone"}`)
	})

	login, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if login != "someone" {
		t.Errorf("login = %q", login)
	}
}
