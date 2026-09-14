package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if err := os.WriteFile(path, []byte("hf_testtoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommitCandidateAsPRSendsOneNDJSONCommitAndReturnsPRURL(t *testing.T) {
	dir := t.TempDir()
	candidatePath := writeTestCandidate(t, dir)
	candidateBytes, err := os.ReadFile(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var candidate ContributionCandidate
	if err := json.Unmarshal(candidateBytes, &candidate); err != nil {
		t.Fatal(err)
	}
	requests := 0
	var gotAuth, gotPath, gotContentType string
	var gotLines []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err == nil {
				gotLines = append(gotLines, decoded)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"pullRequestUrl":"https://huggingface.co/datasets/owner/dataset/discussions/1"}`))
	}))
	defer server.Close()

	commitURL := server.URL + "/api/datasets/owner/dataset/commit/main?create_pr=1"
	prURL, err := commitCandidateAsPR(context.Background(), server.Client(), commitURL, "hf_testtoken", candidate.CandidateID+".json", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly one commit request, got %d", requests)
	}
	if gotAuth != "Bearer hf_testtoken" {
		t.Fatalf("token not forwarded correctly: %q", gotAuth)
	}
	if gotContentType != "application/x-ndjson" {
		t.Fatalf("unexpected content type: %q", gotContentType)
	}
	if gotPath == "" {
		t.Fatal("request never reached the stub server")
	}
	if prURL != "https://huggingface.co/datasets/owner/dataset/discussions/1" {
		t.Fatalf("pull request URL not parsed from response: %q", prURL)
	}
	if len(gotLines) != 2 || gotLines[0]["key"] != "header" || gotLines[1]["key"] != "file" {
		t.Fatalf("commit body is not header+file NDJSON: %#v", gotLines)
	}
	fileValue, _ := gotLines[1]["value"].(map[string]any)
	decoded, err := base64.StdEncoding.DecodeString(fileValue["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var sentCandidate ContributionCandidate
	if err := json.Unmarshal(decoded, &sentCandidate); err != nil {
		t.Fatal(err)
	}
	if sentCandidate.CandidateID != candidate.CandidateID || sentCandidate.SchemaVersion != candidate.SchemaVersion {
		t.Fatalf("committed body does not match the prepared candidate: %#v", sentCandidate)
	}
}

func TestCommitCandidateAsPRFailsOnNonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer server.Close()
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL+"/commit", "bad-token", candidate.CandidateID+".json", candidate); err == nil {
		t.Fatal("expected an error on a non-2xx commit response")
	}
}

func TestCommitCandidateAsPRFailsWithoutPullRequestURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL+"/commit", "hf_testtoken", candidate.CandidateID+".json", candidate); err == nil {
		t.Fatal("expected an error when the API doesn't report a pull request URL (would mean a direct commit happened)")
	}
}

func TestSubmitContributionCandidateRejectsNonCandidateFile(t *testing.T) {
	dir := t.TempDir()
	notACandidate := filepath.Join(dir, "not-a-candidate.json")
	if err := os.WriteFile(notACandidate, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := writeTestToken(t, dir)
	if _, err := SubmitContributionCandidate(context.Background(), http.DefaultClient, notACandidate, tokenPath, "owner/dataset", "main"); err == nil {
		t.Fatal("expected rejection of a file that is not a contribution candidate")
	}
}

func TestSubmitContributionCandidateRejectsGroupReadableToken(t *testing.T) {
	dir := t.TempDir()
	candidatePath := writeTestCandidate(t, dir)
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("hf_testtoken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SubmitContributionCandidate(context.Background(), http.DefaultClient, candidatePath, tokenPath, "owner/dataset", "main"); err == nil {
		t.Fatal("expected rejection of a group/other readable token file")
	}
}

func TestSubmitContributionCandidateRejectsBadRepoPattern(t *testing.T) {
	dir := t.TempDir()
	candidatePath := writeTestCandidate(t, dir)
	tokenPath := writeTestToken(t, dir)
	if _, err := SubmitContributionCandidate(context.Background(), http.DefaultClient, candidatePath, tokenPath, "not-a-valid-repo", "main"); err == nil {
		t.Fatal("expected rejection of a malformed dataset repo")
	}
}
