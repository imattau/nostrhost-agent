package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const maxRuntimeConfigBytes = 256 * 1024

type RuntimeFileConfig struct {
	Relay               RelayFileConfig        `json:"relay"`
	Inference           InferenceFileConfig    `json:"inference,omitempty"`
	Embeddings          EmbeddingFileConfig    `json:"embeddings,omitempty"`
	Policy              Policy                 `json:"policy"`
	ObservationQueries  []ObservationQuery     `json:"observation_queries"`
	VerificationRules   []VerificationRule     `json:"verification_rules,omitempty"`
	KnowledgeCorpusPath string                 `json:"knowledge_corpus_path,omitempty"`
	AuditPath           string                 `json:"audit_path"`
	Interval            string                 `json:"interval,omitempty"`
	RunImmediately      bool                   `json:"run_immediately,omitempty"`
	ListenForEvents     bool                   `json:"listen_for_events,omitempty"`
	EventLookback       string                 `json:"event_lookback,omitempty"`
	Contribution        ContributionFileConfig `json:"contribution,omitempty"`
}

// ContributionFileConfig is the resident daemon's own, operator-set
// contribution config. When Enabled, the daemon submits every completed
// cycle as a Hugging Face pull request itself, with no per-cycle human
// review -- see ContributionAutoSubmitter. Off by default.
type ContributionFileConfig struct {
	Enabled      bool   `json:"enabled,omitempty"`
	DatasetRepo  string `json:"dataset_repo,omitempty"`
	TokenPath    string `json:"token_path,omitempty"`
	BaseRevision string `json:"base_revision,omitempty"`
	StatePath    string `json:"state_path,omitempty"`
}

type RelayFileConfig struct {
	RelayURL         string `json:"relay_url"`
	AgentSecretKey   string `json:"agent_secret_key"`
	TrustedServerKey string `json:"trusted_server_key"`
	TargetPubkey     string `json:"target_pubkey,omitempty"`
	ResultTimeout    string `json:"result_timeout,omitempty"`
}

type InferenceFileConfig struct {
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

type EmbeddingFileConfig struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key,omitempty"`
}

// LoadRuntimeConfig reads strict JSON configuration from a regular file that
// is not accessible to group or other users. Private relay and inference keys
// may be present, so broad file permissions and symlinks are rejected.
func LoadRuntimeConfig(path string) (RuntimeConfig, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("inspect runtime config: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return RuntimeConfig{}, errors.New("runtime config must be a regular file, not a symlink")
	}
	if before.Mode().Perm()&0o077 != 0 {
		return RuntimeConfig{}, errors.New("runtime config must not be readable or writable by group or other users")
	}
	file, err := os.Open(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("open runtime config: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm()&0o077 != 0 {
		return RuntimeConfig{}, errors.New("runtime config changed while it was being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeConfigBytes+1))
	if err != nil || len(data) > maxRuntimeConfigBytes {
		return RuntimeConfig{}, errors.New("runtime config exceeds the 256 KiB limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config RuntimeFileConfig
	if err := decoder.Decode(&config); err != nil {
		return RuntimeConfig{}, errors.New("runtime config contains invalid JSON or unknown fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return RuntimeConfig{}, errors.New("runtime config must contain one JSON value")
	}
	interval, err := parseOptionalDuration(config.Interval, "interval")
	if err != nil {
		return RuntimeConfig{}, err
	}
	resultTimeout, err := parseOptionalDuration(config.Relay.ResultTimeout, "relay.result_timeout")
	if err != nil {
		return RuntimeConfig{}, err
	}
	eventLookback, err := parseOptionalDuration(config.EventLookback, "event_lookback")
	if err != nil || eventLookback > 24*time.Hour {
		return RuntimeConfig{}, errors.New("event_lookback must be a duration between zero and 24 hours")
	}
	var inference LLMPlannerConfig
	if config.Policy.Level != Observe {
		inference = LLMPlannerConfig{
			BaseURL: config.Inference.BaseURL, Model: config.Inference.Model,
			APIKey: config.Inference.APIKey, MaxBytes: config.Inference.MaxBytes,
		}
	}
	if (config.Embeddings.BaseURL != "" || config.Embeddings.Model != "" || config.Embeddings.APIKey != "") &&
		(config.Embeddings.BaseURL == "" || config.Embeddings.Model == "") {
		return RuntimeConfig{}, errors.New("embeddings.base_url and embeddings.model must both be set when semantic embeddings are enabled")
	}
	embeddings := EmbeddingConfig{
		BaseURL: config.Embeddings.BaseURL, Model: config.Embeddings.Model, APIKey: config.Embeddings.APIKey,
	}
	contribution := config.Contribution
	if contribution.Enabled {
		if !modelRepositoryPattern.MatchString(contribution.DatasetRepo) {
			return RuntimeConfig{}, errors.New("contribution.dataset_repo must look like <owner>/<name> when contribution.enabled is true")
		}
		if contribution.TokenPath == "" {
			contribution.TokenPath = "/etc/nostrhost-agent/hf_token"
		}
		if contribution.BaseRevision == "" {
			contribution.BaseRevision = "main"
		}
		if contribution.StatePath == "" {
			contribution.StatePath = "/var/lib/nostrhost-agent/contribution-submitted.jsonl"
		}
	}
	return RuntimeConfig{
		Relay: RelayTransportConfig{
			RelayURL: config.Relay.RelayURL, AgentSecretKey: config.Relay.AgentSecretKey,
			TrustedServerKey: config.Relay.TrustedServerKey, TargetPubkey: config.Relay.TargetPubkey,
			ResultTimeout: resultTimeout,
		},
		Inference: inference, Embeddings: embeddings, Policy: config.Policy,
		ObservationQueries: config.ObservationQueries, VerificationRules: config.VerificationRules,
		KnowledgeCorpusPath: config.KnowledgeCorpusPath, AuditPath: config.AuditPath,
		Interval: interval, RunImmediately: config.RunImmediately,
		ListenForEvents: config.ListenForEvents, EventLookback: eventLookback,
		Contribution: contribution,
	}, nil
}

func parseOptionalDuration(value, name string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("%s must be a non-negative Go duration", name)
	}
	return duration, nil
}
