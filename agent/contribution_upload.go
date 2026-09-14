package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const maxContributionCandidateBytes = 4 << 20

// ContributionSubmission is the result of successfully proposing one
// already locally-redacted candidate file as a pull request against a
// Hugging Face dataset repository. This is the only place in
// nostrhost-agent that ever transmits contribution data, and it never runs
// on its own — only an explicit operator action invokes it. It never
// commits directly to the target branch: every submission is a PR, so a
// maintainer reviews it before it becomes part of the dataset, matching the
// "submitted, not trusted" model in the community contribution loop.
type ContributionSubmission struct {
	Repo           string `json:"repo"`
	Path           string `json:"path"`
	BaseRevision   string `json:"base_revision"`
	PullRequestURL string `json:"pull_request_url"`
}

// SubmitContributionCandidate opens a pull request adding exactly one file —
// the candidate the operator explicitly prepared and reviewed via
// nostrhost-agent-export — against a Hugging Face dataset repo the operator
// configured. It refuses to run against anything that doesn't look like a
// candidate this package produced, reads the token from a root-only file
// (never argv/env), and does not touch the source audit journal or any
// other file.
func SubmitContributionCandidate(ctx context.Context, client *http.Client, candidatePath, tokenPath, repo, baseRevision string) (ContributionSubmission, error) {
	if baseRevision == "" {
		baseRevision = "main"
	}
	if !modelRepositoryPattern.MatchString(repo) {
		return ContributionSubmission{}, errors.New("dataset repo must look like <owner>/<name>")
	}
	candidate, err := readVerifiedCandidate(candidatePath)
	if err != nil {
		return ContributionSubmission{}, err
	}
	token, err := readSecretFile(tokenPath)
	if err != nil {
		return ContributionSubmission{}, err
	}
	remotePath := candidate.CandidateID + ".json"
	commitURL := fmt.Sprintf("https://huggingface.co/api/datasets/%s/commit/%s?create_pr=1", repo, baseRevision)
	pullRequestURL, err := commitCandidateAsPR(ctx, client, commitURL, token, remotePath, candidate)
	if err != nil {
		return ContributionSubmission{}, err
	}
	return ContributionSubmission{Repo: repo, Path: remotePath, BaseRevision: baseRevision, PullRequestURL: pullRequestURL}, nil
}

// commitCandidateAsPR sends a single-file addition through Hugging Face's
// NDJSON "create commit" API with create_pr=1, so the write lands as a pull
// request rather than a direct commit to baseRevision. Returns the created
// PR's URL from the response.
func commitCandidateAsPR(ctx context.Context, client *http.Client, commitURL, token, remotePath string, candidate ContributionCandidate) (string, error) {
	data, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode contribution candidate: %w", err)
	}
	header, err := json.Marshal(map[string]any{
		"key": "header",
		"value": map[string]any{
			"summary": "Add contribution candidate " + candidate.CandidateID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode commit header: %w", err)
	}
	fileOp, err := json.Marshal(map[string]any{
		"key": "file",
		"value": map[string]any{
			"path":     remotePath,
			"content":  base64.StdEncoding.EncodeToString(data),
			"encoding": "base64",
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode commit file operation: %w", err)
	}
	body := append(append(header, '\n'), fileOp...)
	body = append(body, '\n')
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, commitURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create commit request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-ndjson")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("submit contribution candidate: %w", err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("hugging face commit API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var result struct {
		PullRequestURL string `json:"pullRequestUrl"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || result.PullRequestURL == "" {
		return "", fmt.Errorf("hugging face commit succeeded but did not report a pull request URL: %s", strings.TrimSpace(string(responseBody)))
	}
	return result.PullRequestURL, nil
}

func readVerifiedCandidate(path string) (ContributionCandidate, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ContributionCandidate{}, fmt.Errorf("inspect contribution candidate: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ContributionCandidate{}, errors.New("contribution candidate must be a regular file, not a symlink")
	}
	if info.Size() > maxContributionCandidateBytes {
		return ContributionCandidate{}, errors.New("contribution candidate is larger than expected")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ContributionCandidate{}, fmt.Errorf("read contribution candidate: %w", err)
	}
	var candidate ContributionCandidate
	if err := json.Unmarshal(data, &candidate); err != nil {
		return ContributionCandidate{}, errors.New("contribution candidate is not valid JSON")
	}
	if candidate.SchemaVersion != contributionSchemaVersion || candidate.CandidateID == "" || candidate.ReviewWarning == "" {
		return ContributionCandidate{}, errors.New("file does not look like a nostrhost-agent-export contribution candidate")
	}
	return candidate, nil
}

func readSecretFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect token file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("token file must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("token file has group or other permissions; secure it to mode 0600 first")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("token file is empty")
	}
	return token, nil
}
