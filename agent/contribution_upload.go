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
	"net/url"
	"strings"
)

const maxContributionCandidateBytes = 4 << 20

// contributionsDatasetPath is the single shared file every contribution is
// appended to as one JSON line, instead of each contributor's candidate
// becoming its own file. Community-scale contribution (many hosts, many
// cycles each) would otherwise generate one small file per candidate with no
// natural place to consolidate them.
const contributionsDatasetPath = "contributions.jsonl"

// maxContributionsDatasetBytes bounds how much of the existing shared file
// this process will fetch and hold in memory before appending to it.
const maxContributionsDatasetBytes = 64 << 20

const (
	githubAPIVersion      = "2022-11-28"
	contributionUserAgent = "nostrhost-agent"
)

// Contributions are opened as pull requests against the community GitHub
// repository (imattau/nostrhost-contributions by default), not pushed
// straight to the Hugging Face dataset. That repository's `validate` CI runs
// the append-only/redaction checks, merges the PR automatically, and its
// `sync-to-huggingface` workflow copies the shared file to the dataset. A
// direct Hub commit would bypass that validation gate entirely and its
// pull requests have no merge queue, so they never merge.
type ContributionSubmission struct {
	Repo           string `json:"repo"`
	Path           string `json:"path"`
	BaseRevision   string `json:"base_revision"`
	PullRequestURL string `json:"pull_request_url"`
}

// SubmitContributionCandidate opens a pull request appending exactly one
// line — the candidate the operator explicitly prepared and reviewed via
// nostrhost-agent-export — to the shared contributions file in a GitHub
// repository the operator configured. It refuses to run against anything
// that doesn't look like a candidate this package produced, reads the token
// from a root-only file (never argv/env), and does not touch the source audit
// journal or any other file.
func SubmitContributionCandidate(ctx context.Context, client *http.Client, candidatePath, tokenPath, repo, baseRevision string) (ContributionSubmission, error) {
	if baseRevision == "" {
		baseRevision = "main"
	}
	if !modelRepositoryPattern.MatchString(repo) {
		return ContributionSubmission{}, errors.New("contribution repo must look like <owner>/<name>")
	}
	candidate, err := readVerifiedCandidate(candidatePath)
	if err != nil {
		return ContributionSubmission{}, err
	}
	pullRequestURL, err := submitCandidatePR(ctx, client, defaultGitHubAPIBaseURL, tokenPath, repo, baseRevision, candidate)
	if err != nil {
		return ContributionSubmission{}, err
	}
	return ContributionSubmission{Repo: repo, Path: contributionsDatasetPath, BaseRevision: baseRevision, PullRequestURL: pullRequestURL}, nil
}

// githubRequest builds an authenticated GitHub REST request. Every GitHub
// endpoint requires a User-Agent, and the API version/accept headers pin the
// JSON response shape this file parses.
func githubRequest(ctx context.Context, method, requestURL, token string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	request.Header.Set("User-Agent", contributionUserAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func githubDo(client *http.Client, request *http.Request) (int, []byte, error) {
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxContributionsDatasetBytes+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}

func responseSnippet(body []byte) string {
	const limit = 512
	if len(body) > limit {
		body = body[:limit]
	}
	return strings.TrimSpace(string(body))
}

func githubError(what string, status int, body []byte) error {
	return fmt.Errorf("github %s returned HTTP %d: %s", what, status, responseSnippet(body))
}

// githubBaseRefSHA resolves the commit the base branch currently points at,
// which the new contribution branch is created from.
func githubBaseRefSHA(ctx context.Context, client *http.Client, apiBaseURL, repo, branch, token string) (string, error) {
	requestURL := fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", apiBaseURL, repo, url.PathEscape(branch))
	request, err := githubRequest(ctx, http.MethodGet, requestURL, token, nil)
	if err != nil {
		return "", fmt.Errorf("create base ref request: %w", err)
	}
	status, body, err := githubDo(client, request)
	if err != nil {
		return "", fmt.Errorf("resolve base ref: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", githubError("base ref", status, body)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil || ref.Object.SHA == "" {
		return "", errors.New("github base ref response did not contain a commit sha")
	}
	return ref.Object.SHA, nil
}

// fetchExistingContributionFile reads the shared file at baseRevision so the
// new candidate is appended rather than overwriting it, and returns the blob
// sha the Contents API needs to update it. A file that does not exist yet (the
// repository's first contribution) is not an error — it just means we create
// it.
func fetchExistingContributionFile(ctx context.Context, client *http.Client, apiBaseURL, repo, branch, token string) ([]byte, string, error) {
	requestURL := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s",
		apiBaseURL, repo, contributionsDatasetPath, url.QueryEscape(branch))
	request, err := githubRequest(ctx, http.MethodGet, requestURL, token, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create dataset file fetch request: %w", err)
	}
	status, body, err := githubDo(client, request)
	if err != nil {
		return nil, "", fmt.Errorf("fetch existing contributions file: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, "", nil
	}
	if status < 200 || status >= 300 {
		return nil, "", githubError("contents", status, body)
	}
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	if err := json.Unmarshal(body, &content); err != nil {
		return nil, "", fmt.Errorf("decode github contents response: %w", err)
	}
	if content.Encoding != "base64" {
		return nil, "", fmt.Errorf("unexpected github content encoding %q", content.Encoding)
	}
	// GitHub wraps the base64 payload with newlines every 60 characters.
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, content.Content)
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, "", fmt.Errorf("decode existing contributions file: %w", err)
	}
	if len(decoded) > maxContributionsDatasetBytes {
		return nil, "", errors.New("shared contributions file is larger than this process will hold in memory")
	}
	if len(decoded) > 0 && decoded[len(decoded)-1] != '\n' {
		decoded = append(decoded, '\n')
	}
	return decoded, content.SHA, nil
}

func createContributionBranch(ctx context.Context, client *http.Client, apiBaseURL, repo, branch, baseSHA, token string) error {
	payload, err := json.Marshal(map[string]string{"ref": "refs/heads/" + branch, "sha": baseSHA})
	if err != nil {
		return fmt.Errorf("encode branch request: %w", err)
	}
	requestURL := fmt.Sprintf("%s/repos/%s/git/refs", apiBaseURL, repo)
	request, err := githubRequest(ctx, http.MethodPost, requestURL, token, payload)
	if err != nil {
		return fmt.Errorf("create branch request: %w", err)
	}
	status, body, err := githubDo(client, request)
	if err != nil {
		return fmt.Errorf("create contribution branch: %w", err)
	}
	if status < 200 || status >= 300 {
		return githubError("create branch", status, body)
	}
	return nil
}

func putContributionFile(ctx context.Context, client *http.Client, apiBaseURL, repo, branch, fileSHA string, content []byte, message, token string) error {
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if fileSHA != "" {
		payload["sha"] = fileSHA
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode file request: %w", err)
	}
	requestURL := fmt.Sprintf("%s/repos/%s/contents/%s", apiBaseURL, repo, contributionsDatasetPath)
	request, err := githubRequest(ctx, http.MethodPut, requestURL, token, encoded)
	if err != nil {
		return fmt.Errorf("create file commit request: %w", err)
	}
	status, body, err := githubDo(client, request)
	if err != nil {
		return fmt.Errorf("commit contribution file: %w", err)
	}
	if status < 200 || status >= 300 {
		return githubError("contents commit", status, body)
	}
	return nil
}

func openContributionPullRequest(ctx context.Context, client *http.Client, apiBaseURL, repo, title, head, base, token string) (string, string, error) {
	payload, err := json.Marshal(map[string]string{
		"title": title,
		"head":  head,
		"base":  base,
		"body": "Automated nostrhost-agent contribution. The candidate was redacted on the host " +
			"before submission; this pull request is opened only so the repository's validation " +
			"workflow can run before the automatic merge.",
	})
	if err != nil {
		return "", "", fmt.Errorf("encode pull request: %w", err)
	}
	requestURL := fmt.Sprintf("%s/repos/%s/pulls", apiBaseURL, repo)
	request, err := githubRequest(ctx, http.MethodPost, requestURL, token, payload)
	if err != nil {
		return "", "", fmt.Errorf("create pull request request: %w", err)
	}
	status, body, err := githubDo(client, request)
	if err != nil {
		return "", "", fmt.Errorf("open contribution pull request: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", "", githubError("pull request", status, body)
	}
	var result struct {
		HTMLURL string `json:"html_url"`
		NodeID  string `json:"node_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.HTMLURL == "" {
		return "", "", errors.New("github pull request succeeded but did not report an html_url")
	}
	return result.HTMLURL, result.NodeID, nil
}

// enableContributionAutoMerge turns on "merge when checks pass" for the new
// pull request. GitHub only auto-merges a pull request whose auto-merge has
// been enabled (per PR, via GraphQL); without this the PR would sit open
// forever, exactly like a direct Hub pull request. The repository's branch
// protection supplies the required `validate` check.
func enableContributionAutoMerge(ctx context.Context, client *http.Client, apiBaseURL, nodeID, token string) error {
	if nodeID == "" {
		return errors.New("cannot enable auto-merge without a pull request node id")
	}
	const mutation = `mutation EnableContributionAutoMerge($id: ID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: SQUASH}) {
    pullRequest { number }
  }
}`
	payload, err := json.Marshal(map[string]any{
		"query":     mutation,
		"variables": map[string]string{"id": nodeID},
	})
	if err != nil {
		return fmt.Errorf("encode auto-merge request: %w", err)
	}
	requestURL := fmt.Sprintf("%s/graphql", apiBaseURL)
	request, err := githubRequest(ctx, http.MethodPost, requestURL, token, payload)
	if err != nil {
		return fmt.Errorf("create auto-merge request: %w", err)
	}
	status, body, err := githubDo(client, request)
	if err != nil {
		return fmt.Errorf("enable auto-merge: %w", err)
	}
	if status < 200 || status >= 300 {
		return githubError("auto-merge", status, body)
	}
	var result struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode auto-merge response: %w", err)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("github auto-merge rejected: %s", result.Errors[0].Message)
	}
	return nil
}

// commitCandidateAsPR creates a short-lived branch from baseRevision, appends
// the candidate as one compact JSON line to the shared contributions file on
// that branch, and opens a pull request back to baseRevision. Nothing is ever
// committed directly to the base branch.
func commitCandidateAsPR(ctx context.Context, client *http.Client, apiBaseURL, repo, baseRevision, token string, candidate ContributionCandidate) (string, error) {
	line, err := json.Marshal(candidate)
	if err != nil {
		return "", fmt.Errorf("encode contribution candidate: %w", err)
	}
	baseSHA, err := githubBaseRefSHA(ctx, client, apiBaseURL, repo, baseRevision, token)
	if err != nil {
		return "", err
	}
	existing, fileSHA, err := fetchExistingContributionFile(ctx, client, apiBaseURL, repo, baseRevision, token)
	if err != nil {
		return "", err
	}
	updated := append(existing, line...)
	updated = append(updated, '\n')
	branch := "contribution-" + candidate.CandidateID
	if err := createContributionBranch(ctx, client, apiBaseURL, repo, branch, baseSHA, token); err != nil {
		return "", err
	}
	title := "Add contribution candidate " + candidate.CandidateID
	if err := putContributionFile(ctx, client, apiBaseURL, repo, branch, fileSHA, updated, title, token); err != nil {
		return "", err
	}
	prURL, nodeID, err := openContributionPullRequest(ctx, client, apiBaseURL, repo, title, branch, baseRevision, token)
	if err != nil {
		return "", err
	}
	if err := enableContributionAutoMerge(ctx, client, apiBaseURL, nodeID, token); err != nil {
		return "", err
	}
	return prURL, nil
}

func readVerifiedCandidate(path string) (ContributionCandidate, error) {
	file, _, err := openVerifiedFile(path, WithMaxOpenSize(maxContributionCandidateBytes))
	if err != nil {
		return ContributionCandidate{}, fmt.Errorf("inspect contribution candidate: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxContributionCandidateBytes+1))
	if err != nil || int64(len(data)) > maxContributionCandidateBytes {
		return ContributionCandidate{}, errors.New("contribution candidate is larger than expected")
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

// maxSecretFileBytes bounds how much of a token/secret file this process
// will hold in memory. Legitimate tokens are short; this is a defense-in-
// depth cap, not a real-world limit.
const maxSecretFileBytes = 64 * 1024

func readSecretFile(path string) (string, error) {
	file, _, err := openVerifiedFile(path, WithRejectGroupOtherPerms(), WithMaxOpenSize(maxSecretFileBytes))
	if err != nil {
		return "", fmt.Errorf("inspect token file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil || len(data) > maxSecretFileBytes {
		return "", errors.New("token file is larger than expected")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("token file is empty")
	}
	return token, nil
}

// submitCandidatePR reads the operator-configured GitHub token from tokenPath
// and submits candidate as a pull request against repo. This is the shared
// "read secret, call the API" sequence used by both the manual
// (SubmitContributionCandidate) and automatic (ContributionAutoSubmitter)
// submission paths -- the only difference between them is how the caller
// builds ctx.
func submitCandidatePR(ctx context.Context, client *http.Client, apiBaseURL, tokenPath, repo, baseRevision string, candidate ContributionCandidate) (string, error) {
	token, err := readSecretFile(tokenPath)
	if err != nil {
		return "", err
	}
	return commitCandidateAsPR(ctx, client, apiBaseURL, repo, baseRevision, token, candidate)
}
