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
	var gotAuth, gotContentType string
	var gotCommitPath bool
	var gotLines []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method == http.MethodGet {
			// No prior contributions file yet.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotCommitPath = strings.Contains(r.URL.Path, "/commit/")
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

	prURL, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/dataset", "main", "hf_testtoken", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expected one fetch of the existing shared file plus one commit request, got %d", requests)
	}
	if gotAuth != "Bearer hf_testtoken" {
		t.Fatalf("token not forwarded correctly: %q", gotAuth)
	}
	if gotContentType != "application/x-ndjson" {
		t.Fatalf("unexpected content type: %q", gotContentType)
	}
	if !gotCommitPath {
		t.Fatal("commit request never reached the stub server")
	}
	if prURL != "https://huggingface.co/datasets/owner/dataset/discussions/1" {
		t.Fatalf("pull request URL not parsed from response: %q", prURL)
	}
	if len(gotLines) != 2 || gotLines[0]["key"] != "header" || gotLines[1]["key"] != "file" {
		t.Fatalf("commit body is not header+file NDJSON: %#v", gotLines)
	}
	fileValue, _ := gotLines[1]["value"].(map[string]any)
	if fileValue["path"] != contributionsDatasetPath {
		t.Fatalf("expected the candidate to be committed to the shared file %q, got %q", contributionsDatasetPath, fileValue["path"])
	}
	decoded, err := base64.StdEncoding.DecodeString(fileValue["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one appended line for the first contribution, got %d", len(lines))
	}
	var sentCandidate ContributionCandidate
	if err := json.Unmarshal([]byte(lines[0]), &sentCandidate); err != nil {
		t.Fatal(err)
	}
	if sentCandidate.CandidateID != candidate.CandidateID || sentCandidate.SchemaVersion != candidate.SchemaVersion {
		t.Fatalf("committed body does not match the prepared candidate: %#v", sentCandidate)
	}
}

func TestCommitCandidateAsPRAppendsToExistingSharedFile(t *testing.T) {
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
	existingLine := `{"schema_version":"nostrhost-agent-contribution/v1","candidate_id":"candidate-existing"}` + "\n"
	var gotContent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(existingLine))
			return
		}
		body, _ := io.ReadAll(r.Body)
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				continue
			}
			if decoded["key"] == "file" {
				value, _ := decoded["value"].(map[string]any)
				gotContent, _ = base64.StdEncoding.DecodeString(value["content"].(string))
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"pullRequestUrl":"https://huggingface.co/datasets/owner/dataset/discussions/2"}`))
	}))
	defer server.Close()

	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/dataset", "main", "hf_testtoken", candidate); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(gotContent)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected the existing line plus the new candidate's line, got %d lines: %q", len(lines), gotContent)
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer server.Close()
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/dataset", "main", "bad-token", candidate); err == nil {
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
	if _, err := commitCandidateAsPR(context.Background(), server.Client(), server.URL, "owner/dataset", "main", "hf_testtoken", candidate); err == nil {
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
