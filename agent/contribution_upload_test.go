package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeTestCandidate(t *testing.T, dir string) string {
	t.Helper()
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "candidate.json")
	if err := WriteContributionCandidate(path, candidate); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestToken(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("gh_testtoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type githubCapture struct {
	mu         sync.Mutex
	calls      []string
	auth       string
	refCreated map[string]any
	filePut    map[string]any
	pull       map[string]any
	graphql    map[string]any
}

func (c *githubCapture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	if c.auth == "" {
		c.auth = r.Header.Get("Authorization")
	}
}

// newGitHubServer emulates the subset of the GitHub REST API the submission
// path uses. An empty existingContent makes the contents endpoint return 404,
// meaning the shared file does not exist yet.
func newGitHubServer(t *testing.T, capture *githubCapture, existingContent, existingSHA, baseSHA, prURL, failPath string, failStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.record(r)
		if failPath != "" && strings.Contains(r.URL.Path, failPath) {
			if failStatus == 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]string{{"message": "Auto merge is not allowed for this repository"}},
				})
				return
			}
			w.WriteHeader(failStatus)
			_, _ = w.Write([]byte(`{"message":"forced failure"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": baseSHA}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			if existingContent == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			// GitHub wraps the base64 payload with newlines; exercise that.
			wrapped := wrapBase64(base64.StdEncoding.EncodeToString([]byte(existingContent)))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": wrapped, "encoding": "base64", "sha": existingSHA,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			_ = json.NewDecoder(r.Body).Decode(&capture.refCreated)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			_ = json.NewDecoder(r.Body).Decode(&capture.filePut)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			_ = json.NewDecoder(r.Body).Decode(&capture.pull)
			w.WriteHeader(http.StatusCreated)
			if prURL != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"html_url": prURL, "node_id": "PR_kwDOtest"})
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			_ = json.NewDecoder(r.Body).Decode(&capture.graphql)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":1}}}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"unexpected request"}`))
		}
	}))
}

func wrapBase64(value string) string {
	var builder strings.Builder
	for len(value) > 60 {
		builder.WriteString(value[:60])
		builder.WriteString("\n")
		value = value[60:]
	}
	builder.WriteString(value)
	return builder.String()
}

func exportableCandidate(t *testing.T, dir string) ContributionCandidate {
	t.Helper()
	path := writeTestCandidate(t, dir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var candidate ContributionCandidate
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatal(err)
	}
	return candidate
}

func TestCommitCandidateAsPROpensGitHubPRAppendingOneLine(t *testing.T) {
	dir := t.TempDir()
	candidate := exportableCandidate(t, dir)
	const prURL = "https://github.com/imattau/nostrhost-contributions/pull/7"
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, "", "", "base-sha-1", prURL, "", 0)
	defer server.Close()

	got, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/repo", "main", "gh_testtoken", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got != prURL {
		t.Fatalf("pull request URL not returned: %q", got)
	}
	if capture.auth != "Bearer gh_testtoken" {
		t.Fatalf("token not forwarded correctly: %q", capture.auth)
	}
	wantCalls := []string{
		"GET /repos/owner/repo/git/ref/heads/main",
		"GET /repos/owner/repo/contents/contributions.jsonl",
		"POST /repos/owner/repo/git/refs",
		"PUT /repos/owner/repo/contents/contributions.jsonl",
		"POST /repos/owner/repo/pulls",
		"POST /graphql",
	}
	if strings.Join(capture.calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("unexpected GitHub call sequence:\n%s", strings.Join(capture.calls, "\n"))
	}
	if capture.refCreated["ref"] != "refs/heads/contribution-"+candidate.CandidateID {
		t.Fatalf("branch created from the wrong ref/base: %#v", capture.refCreated)
	}
	if capture.refCreated["sha"] != "base-sha-1" {
		t.Fatalf("branch not created from the base commit: %#v", capture.refCreated)
	}
	if capture.filePut["branch"] != "contribution-"+candidate.CandidateID {
		t.Fatalf("file committed to the wrong branch: %#v", capture.filePut)
	}
	if _, present := capture.filePut["sha"]; present {
		t.Fatalf("first contribution must not send a file sha: %#v", capture.filePut)
	}
	decoded, err := base64.StdEncoding.DecodeString(capture.filePut["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one appended line, got %d", len(lines))
	}
	var sent ContributionCandidate
	if err := json.Unmarshal([]byte(lines[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.CandidateID != candidate.CandidateID {
		t.Fatalf("committed body does not match the prepared candidate: %#v", sent)
	}
	if capture.pull["head"] != "contribution-"+candidate.CandidateID || capture.pull["base"] != "main" {
		t.Fatalf("pull request head/base wrong: %#v", capture.pull)
	}
	query, _ := capture.graphql["query"].(string)
	if !strings.Contains(query, "enablePullRequestAutoMerge") {
		t.Fatalf("auto-merge was not enabled for the pull request: %#v", capture.graphql)
	}
}

func TestCommitCandidateAsPRAppendsToExistingSharedFile(t *testing.T) {
	dir := t.TempDir()
	candidate := exportableCandidate(t, dir)
	existing := `{"schema_version":"nostrhost-agent-contribution/v1","candidate_id":"candidate-existing"}` + "\n"
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, existing, "blob-sha-9", "base-sha-2",
		"https://github.com/imattau/nostrhost-contributions/pull/8", "", 0)
	defer server.Close()

	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/repo", "main", "gh_testtoken", candidate); err != nil {
		t.Fatal(err)
	}
	if capture.filePut["sha"] != "blob-sha-9" {
		t.Fatalf("existing file sha not forwarded to the update: %#v", capture.filePut)
	}
	decoded, err := base64.StdEncoding.DecodeString(capture.filePut["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected the existing line plus the new candidate's line, got %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[0], `{"schema_version":"nostrhost-agent-contribution/v1","candidate_id":"candidate-existing"`) {
		t.Fatalf("existing shared file content was not preserved: %q", lines[0])
	}
	var appended ContributionCandidate
	if err := json.Unmarshal([]byte(lines[1]), &appended); err != nil {
		t.Fatal(err)
	}
	if appended.CandidateID != candidate.CandidateID {
		t.Fatalf("new candidate was not appended as the second line: %#v", appended)
	}
}

func TestCommitCandidateAsPRFailsOnNonSuccessStatus(t *testing.T) {
	dir := t.TempDir()
	candidate := exportableCandidate(t, dir)
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, "", "", "base-sha", "", "/pulls", http.StatusUnprocessableEntity)
	defer server.Close()
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/repo", "main", "bad-token", candidate); err == nil {
		t.Fatal("expected an error on a non-2xx pull request response")
	}
}

func TestCommitCandidateAsPRFailsWithoutPullRequestURL(t *testing.T) {
	dir := t.TempDir()
	candidate := exportableCandidate(t, dir)
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, "", "", "base-sha", "", "", 0)
	defer server.Close()
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/repo", "main", "gh_testtoken", candidate); err == nil {
		t.Fatal("expected an error when the API doesn't report a pull request URL")
	}
}

func TestCommitCandidateAsPRFailsWhenAutoMergeRejected(t *testing.T) {
	dir := t.TempDir()
	candidate := exportableCandidate(t, dir)
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, "", "", "base-sha",
		"https://github.com/imattau/nostrhost-contributions/pull/11", "/graphql", 0)
	defer server.Close()
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/repo", "main", "gh_testtoken", candidate); err == nil {
		t.Fatal("expected an error when GitHub rejects enabling auto-merge")
	}
}

func TestSubmitContributionCandidateRejectsNonCandidateFile(t *testing.T) {
	dir := t.TempDir()
	notACandidate := filepath.Join(dir, "not-a-candidate.json")
	if err := os.WriteFile(notACandidate, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := writeTestToken(t, dir)
	if _, err := SubmitContributionCandidate(context.Background(), http.DefaultClient, notACandidate, tokenPath, "owner/repo", "main"); err == nil {
		t.Fatal("expected rejection of a file that is not a contribution candidate")
	}
}

func TestSubmitContributionCandidateRejectsGroupReadableToken(t *testing.T) {
	dir := t.TempDir()
	candidatePath := writeTestCandidate(t, dir)
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("gh_testtoken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SubmitContributionCandidate(context.Background(), http.DefaultClient, candidatePath, tokenPath, "owner/repo", "main"); err == nil {
		t.Fatal("expected rejection of a group/other readable token file")
	}
}

func TestSubmitContributionCandidateRejectsBadRepoPattern(t *testing.T) {
	dir := t.TempDir()
	candidatePath := writeTestCandidate(t, dir)
	tokenPath := writeTestToken(t, dir)
	if _, err := SubmitContributionCandidate(context.Background(), http.DefaultClient, candidatePath, tokenPath, "not-a-valid-repo", "main"); err == nil {
		t.Fatal("expected rejection of a malformed contribution repo")
	}
}
