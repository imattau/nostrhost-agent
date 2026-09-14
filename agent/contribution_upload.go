package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const maxContributionCandidateBytes = 4 << 20

// ContributionSubmission is the result of successfully uploading one already
// locally-redacted candidate file to a Hugging Face dataset repository. This
// is the only place in nostrhost-agent that ever transmits contribution data,
// and it never runs on its own — only an explicit operator action invokes it.
type ContributionSubmission struct {
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
}

// SubmitContributionCandidate uploads exactly one file — the candidate the
// operator explicitly prepared and reviewed via nostrhost-agent-export — to a
// Hugging Face dataset repo the operator configured. It refuses to run
// against anything that doesn't look like a candidate this package produced,
// reads the token from a root-only file (never argv/env), and does not touch
// the source audit journal or any other file.
func SubmitContributionCandidate(ctx context.Context, client *http.Client, candidatePath, tokenPath, repo, revision string) (ContributionSubmission, error) {
	if revision == "" {
		revision = "main"
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
	uploadURL := fmt.Sprintf("https://huggingface.co/api/datasets/%s/upload/%s/%s", repo, revision, remotePath)
	if err := uploadCandidate(ctx, client, uploadURL, token, candidate); err != nil {
		return ContributionSubmission{}, err
	}
	return ContributionSubmission{Repo: repo, Path: remotePath, Revision: revision}, nil
}

func uploadCandidate(ctx context.Context, client *http.Client, uploadURL, token string, candidate ContributionCandidate) error {
	data, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return fmt.Errorf("encode contribution candidate: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("upload contribution candidate: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("hugging face upload returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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
