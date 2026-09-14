package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestUploadCandidateSendsExactlyOneAuthenticatedRequest(t *testing.T) {
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
	var gotBody ContributionCandidate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	uploadURL := server.URL + "/api/datasets/owner/dataset/upload/main/" + candidate.CandidateID + ".json"
	if err := uploadCandidate(context.Background(), server.Client(), uploadURL, "hf_testtoken", candidate); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly one upload request, got %d", requests)
	}
	if gotAuth != "Bearer hf_testtoken" {
		t.Fatalf("token not forwarded correctly: %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("unexpected content type: %q", gotContentType)
	}
	if gotPath == "" {
		t.Fatal("request never reached the stub server")
	}
	if gotBody.CandidateID != candidate.CandidateID || gotBody.SchemaVersion != candidate.SchemaVersion {
		t.Fatalf("uploaded body does not match the prepared candidate: %#v", gotBody)
	}
}

func TestUploadCandidateFailsOnNonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer server.Close()
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	if err := uploadCandidate(context.Background(), server.Client(), server.URL+"/upload", "bad-token", candidate); err == nil {
		t.Fatal("expected an error on a non-2xx upload response")
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
