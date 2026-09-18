package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imattau/nostrhost-agent/agent"
)

// nostrhost-agent-contribute is the only tool in this module that ever
// transmits data off the host. It is never invoked by the resident agent
// daemon — only by an explicit operator/admin action, and only for one
// candidate file the operator already prepared with nostrhost-agent-export
// and chose to submit.
func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nostrhost-agent-contribute: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("nostrhost-agent-contribute", flag.ContinueOnError)
	candidate := flags.String("candidate", "", "path to a contribution candidate written by nostrhost-agent-export")
	tokenFile := flags.String("token-file", "", "root-only file containing the GitHub token")
	repo := flags.String("repo", "", "GitHub repository, e.g. owner/repo")
	baseRevision := flags.String("base-revision", "main", "repository branch to open the pull request against")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *candidate == "" || *tokenFile == "" || *repo == "" {
		return fmt.Errorf("usage: nostrhost-agent-contribute --candidate PATH --token-file PATH --repo OWNER/REPO [--base-revision BRANCH]")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &http.Client{Timeout: 60 * time.Second}
	result, err := agent.SubmitContributionCandidate(ctx, client, *candidate, *tokenFile, *repo, *baseRevision)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(struct {
		Uploaded       bool   `json:"uploaded"`
		Repo           string `json:"repo"`
		Path           string `json:"path"`
		BaseRevision   string `json:"base_revision"`
		PullRequestURL string `json:"pull_request_url"`
		Message        string `json:"message"`
	}{true, result.Repo, result.Path, result.BaseRevision, result.PullRequestURL, "opened a pull request with the reviewed candidate only; no other file on this host was read or transmitted, and nothing was committed directly"})
}
