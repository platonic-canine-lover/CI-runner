// Package forge talks to GitHub: what needs building, and what happened
// when we built it.
//
// Deliberately hand-rolled over net/http rather than a client library. The
// surface used here is four endpoints, the payloads are small, and a
// dependency-free binary is easier to trust on a machine that also runs
// production than one carrying a tree of transitive modules.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// Client is a minimal GitHub REST client scoped to one repository.
type Client struct {
	token string
	repo  string // "owner/name"
	base  string // API root; overridden in tests
	http  *http.Client
}

func New(token, repo string) *Client {
	return &Client{
		token: token,
		repo:  repo,
		base:  apiBase,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Commit is the head of something worth testing.
type Commit struct {
	SHA     string
	Ref     string // "main", or "pr/12" for a pull request
	Title   string
	PullNum int // 0 when this is a branch head
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Read the body before checking the status: GitHub puts the useful part
	// of a failure in there ("Bad credentials", "Resource not accessible"),
	// and a bare status code sends you looking in the wrong place.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// BranchHead returns the current head commit of a branch.
func (c *Client) BranchHead(ctx context.Context, branch string) (Commit, error) {
	var out struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+c.repo+"/commits/"+branch, nil, &out); err != nil {
		return Commit{}, err
	}
	return Commit{SHA: out.SHA, Ref: branch, Title: firstLine(out.Commit.Message)}, nil
}

// OpenPullRequests returns the head commit of every open pull request.
func (c *Client) OpenPullRequests(ctx context.Context) ([]Commit, error) {
	var out []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+c.repo+"/pulls?state=open&per_page=50", nil, &out); err != nil {
		return nil, err
	}
	commits := make([]Commit, 0, len(out))
	for _, pr := range out {
		commits = append(commits, Commit{
			SHA:     pr.Head.SHA,
			Ref:     fmt.Sprintf("pr/%d", pr.Number),
			Title:   pr.Title,
			PullNum: pr.Number,
		})
	}
	return commits, nil
}

// State is a commit status state as GitHub names them.
type State string

const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	StateError   State = "error"
)

// SetStatus reports a result against a commit. Context is the check's name
// as it appears in the GitHub UI, e.g. "warburton-ci/test".
func (c *Client) SetStatus(ctx context.Context, sha string, state State, checkName, description string) error {
	// GitHub truncates descriptions at 140 characters and rejects nothing,
	// so trim here rather than discover it as a silently shortened message.
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	body := map[string]string{
		"state":       string(state),
		"context":     checkName,
		"description": description,
	}
	return c.do(ctx, http.MethodPost, "/repos/"+c.repo+"/statuses/"+sha, body, nil)
}

// Whoami verifies the token works and returns the login it belongs to. Run
// at startup: a bad token should fail loudly at boot, not quietly on the
// first status write an hour later.
func (c *Client) Whoami(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
