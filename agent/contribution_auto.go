package agent

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ContributionAutoSubmitter submits every eligible completed cycle as a
// Hugging Face pull request with no per-cycle human review — the operator
// opts into this explicitly via config.Contribution.Enabled, off by
// default. It tracks which cycle IDs it has already submitted in a local
// state file so a daemon restart, or the same cycle being reported twice,
// never submits the same cycle again.
//
// This is the one place the resident daemon itself ever transmits
// contribution data off the host; it exists only because the operator
// explicitly configured it, and it submits exactly the same
// ContributionCandidate shape (built by BuildContributionCandidate, with
// the same automated redaction) that nostrhost-agent-export/-contribute
// produce for a manual, reviewed submission — automatic mode simply skips
// the human review step in between.
type ContributionAutoSubmitter struct {
	Config ContributionFileConfig
	Client *http.Client

	// hubBaseURL defaults to the real Hugging Face API and is only
	// overridden in tests, so the commit request can be pointed at a local
	// stub server instead of the network.
	hubBaseURL string

	mu        sync.Mutex
	submitted map[string]bool
}

const defaultHubBaseURL = "https://huggingface.co"

// NewContributionAutoSubmitter loads the dedup state file, if any, and
// returns a ready submitter. A missing state file is not an error.
func NewContributionAutoSubmitter(cfg ContributionFileConfig) (*ContributionAutoSubmitter, error) {
	submitter := &ContributionAutoSubmitter{
		Config:     cfg,
		Client:     &http.Client{Timeout: 60 * time.Second},
		hubBaseURL: defaultHubBaseURL,
		submitted:  make(map[string]bool),
	}
	if err := submitter.loadState(); err != nil {
		return nil, err
	}
	return submitter, nil
}

func (s *ContributionAutoSubmitter) loadState() error {
	file, err := os.Open(s.Config.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open contribution state file: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if id := strings.TrimSpace(scanner.Text()); id != "" {
			s.submitted[id] = true
		}
	}
	return scanner.Err()
}

func (s *ContributionAutoSubmitter) markSubmitted(cycleID string) error {
	file, err := os.OpenFile(s.Config.StatePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open contribution state file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(cycleID + "\n"); err != nil {
		return fmt.Errorf("write contribution state file: %w", err)
	}
	return file.Sync()
}

// OnCycle is meant to be composed into RuntimeConfig.OnCycle. It never
// panics and never surfaces an error to the caller — a submission failure
// is logged, and otherwise does not affect the resident cycle loop.
func (s *ContributionAutoSubmitter) OnCycle(trace CycleTrace, cycleErr error) {
	if s == nil || !s.Config.Enabled || cycleErr != nil || !isEligibleForExport(trace) {
		return
	}
	s.mu.Lock()
	alreadySubmitted := s.submitted[trace.ID]
	s.mu.Unlock()
	if alreadySubmitted {
		return
	}
	candidate, err := BuildContributionCandidate(trace)
	if err != nil {
		log.Printf("nostrhost-agent: automatic contribution skipped for cycle %s: %v", trace.ID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	prURL, err := submitCandidatePR(ctx, s.Client, s.hubBaseURL, s.Config.TokenPath, s.Config.DatasetRepo, s.Config.BaseRevision, candidate)
	if err != nil {
		log.Printf("nostrhost-agent: automatic contribution failed for cycle %s: %v", trace.ID, err)
		return
	}
	s.mu.Lock()
	s.submitted[trace.ID] = true
	s.mu.Unlock()
	if err := s.markSubmitted(trace.ID); err != nil {
		log.Printf("nostrhost-agent: submitted cycle %s (%s) but failed to record it locally, it may be resubmitted: %v", trace.ID, prURL, err)
		return
	}
	log.Printf("nostrhost-agent: automatically submitted cycle %s as a pull request: %s", trace.ID, prURL)
}
