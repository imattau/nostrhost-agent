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

// contributionsDatasetPath is the single shared file every contribution is
// appended to as one JSON line, instead of each contributor's candidate
// becoming its own file in the dataset repo. Community-scale contribution
// (many hosts, many cycles each) would otherwise generate one small file
// per candidate with no natural place to consolidate them.
const contributionsDatasetPath = "contributions.jsonl"

// maxContributionsDatasetBytes bounds how much of the existing shared file
// this process will fetch and hold in memory before appending to it.
const maxContributionsDatasetBytes = 64 << 20

// ContributionSubmission is the result of successfully proposing one
// already locally-redacted candidate as an appended line in the shared
// contributions file, via a pull request against a Hugging Face dataset
// repository. This is the only place in nostrhost-agent that ever
// transmits contribution data, and it never runs on its own — only an
// explicit operator action invokes it. It never commits directly to the
// target branch: every submission is a PR, so a maintainer reviews it
// before it becomes part of the dataset, matching the "submitted, not
// trusted" model in the community contribution loop.
type ContributionSubmission struct {
	Repo           string `json:"repo"`
	Path           string `json:"path"`
	BaseRevision   string `json:"base_revision"`
	PullRequestURL string `json:"pull_request_url"`
}

// SubmitContributionCandidate opens a pull request appending exactly one
// line — the candidate the operator explicitly prepared and reviewed via
// nostrhost-agent-export — to the shared contributions file in a Hugging
// Face dataset repo the operator configured. It refuses to run against
// anything that doesn't look like a candidate this package produced, reads
// the token from a root-only file (never argv/env), and does not touch the
// source audit journal or any other file.
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
	pullRequestURL, err := commitCandidateAsPR(ctx, client, defaultHubBaseURL, repo, baseRevision, token, candidate)
	if err != nil {
		return ContributionSubmission{}, err
	}
	return ContributionSubmission{Repo: repo, Path: contributionsDatasetPath, BaseRevision: baseRevision, PullRequestURL: pullRequestURL}, nil
}

// commitCandidateAsPR fetches the current contents of the shared
// contributions file, appends the candidate as one compact JSON line, and
// sends the updated file through Hugging Face's NDJSON "create commit" API
// with create_pr=1, so the write lands as a pull request rather than a
// direct commit to baseRevision. Returns the created PR's URL from the
// response.
func commitCandidateAsPR(ctx context.Context, client *http.Client, hubBaseURL, repo, baseRevision, token string, candidate ContributionCandidate) (string, error) {
	existing, err := fetchExistingDatasetFile(ctx, client, hubBaseURL, repo, baseRevision, token)
	if err != nil {
		return "", err
	}
	line, err := json.Marshal(candidate)
	if err != nil {
		return "", fmt.Errorf("encode contribution candidate: %w", err)
	}
	updated := append(existing, line...)
	updated = append(updated, '\n')

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
			"path":     contributionsDatasetPath,
			"content":  base64.StdEncoding.EncodeToString(updated),
			"encoding": "base64",
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode commit file operation: %w", err)
	}
	body := append(append(header, '\n'), fileOp...)
	body = append(body, '\n')
	commitURL := fmt.Sprintf("%s/api/datasets/%s/commit/%s?create_pr=1", hubBaseURL, repo, baseRevision)
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

// fetchExistingDatasetFile reads the current contents of the shared
// contributions file at baseRevision, so the new candidate can be appended
// to it rather than overwriting it. A file that doesn't exist yet (the
// dataset's first contribution) is not an error — it just means we start
// from an empty file.
func fetchExistingDatasetFile(ctx context.Context, client *http.Client, hubBaseURL, repo, baseRevision, token string) ([]byte, error) {
	rawURL := fmt.Sprintf("%s/datasets/%s/raw/%s/%s", hubBaseURL, repo, baseRevision, contributionsDatasetPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create dataset file fetch request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch existing contributions file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxContributionsDatasetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read existing contributions file: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("hugging face raw file API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(body) > maxContributionsDatasetBytes {
		return nil, errors.New("shared contributions file is larger than this process will hold in memory")
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	return body, nil
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
