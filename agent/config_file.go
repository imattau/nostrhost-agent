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
	Relay               RelayFileConfig     `json:"relay"`
	Inference           InferenceFileConfig `json:"inference,omitempty"`
	Policy              Policy              `json:"policy"`
	ObservationQueries  []ObservationQuery  `json:"observation_queries"`
	VerificationRules   []VerificationRule  `json:"verification_rules,omitempty"`
	KnowledgeCorpusPath string              `json:"knowledge_corpus_path,omitempty"`
	AuditPath           string              `json:"audit_path"`
	Interval            string              `json:"interval,omitempty"`
	RunImmediately      bool                `json:"run_immediately,omitempty"`
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
	var inference LLMPlannerConfig
	if config.Policy.Level != Observe {
		inference = LLMPlannerConfig{
			BaseURL: config.Inference.BaseURL, Model: config.Inference.Model,
			APIKey: config.Inference.APIKey, MaxBytes: config.Inference.MaxBytes,
		}
	}
	return RuntimeConfig{
		Relay: RelayTransportConfig{
			RelayURL: config.Relay.RelayURL, AgentSecretKey: config.Relay.AgentSecretKey,
			TrustedServerKey: config.Relay.TrustedServerKey, TargetPubkey: config.Relay.TargetPubkey,
			ResultTimeout: resultTimeout,
		},
		Inference: inference, Policy: config.Policy,
		ObservationQueries: config.ObservationQueries, VerificationRules: config.VerificationRules,
		KnowledgeCorpusPath: config.KnowledgeCorpusPath, AuditPath: config.AuditPath,
		Interval: interval, RunImmediately: config.RunImmediately,
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
