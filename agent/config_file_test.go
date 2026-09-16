package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validRuntimeConfigJSON = `{
  "schema_version": 2,
  "relay": {
    "relay_url": "ws://127.0.0.1:4848",
    "agent_secret_key": "1111111111111111111111111111111111111111111111111111111111111111",
    "trusted_server_key": "2222222222222222222222222222222222222222222222222222222222222222",
    "result_timeout": "90s"
  },
  "policy": {"level": "observe", "scopes": {"services.read": true}},
  "observation_queries": [{"operation": "service.status"}],
  "audit_path": "/var/lib/nostrhost-agent/audit.jsonl",
	"interval": "6h",
	"listen_for_events": true,
	"event_lookback": "15m",
	"embeddings": {"base_url": "http://127.0.0.1:8081/v1", "model": "local-embed"}
}`

func writeRuntimeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRuntimeConfigParsesPrivateFileAndDurations(t *testing.T) {
	path := writeRuntimeConfig(t, validRuntimeConfigJSON, 0o600)
	config, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Interval != 6*time.Hour || config.Relay.ResultTimeout != 90*time.Second || config.EventLookback != 15*time.Minute {
		t.Fatalf("runtime durations were not parsed: interval=%s timeout=%s lookback=%s", config.Interval, config.Relay.ResultTimeout, config.EventLookback)
	}
	if config.Policy.Level != Observe || len(config.ObservationQueries) != 1 || config.Inference.Model != "" || !config.ListenForEvents || config.Embeddings.Model != "local-embed" {
		t.Fatalf("unexpected observe-mode configuration: %#v", config)
	}
}

func TestLoadRuntimeConfigRequiresPrivateRegularStrictJSON(t *testing.T) {
	t.Run("broad permissions", func(t *testing.T) {
		path := writeRuntimeConfig(t, validRuntimeConfigJSON, 0o644)
		if _, err := LoadRuntimeConfig(path); err == nil {
			t.Fatal("group-readable config accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := writeRuntimeConfig(t, validRuntimeConfigJSON, 0o600)
		link := filepath.Join(t.TempDir(), "config-link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRuntimeConfig(link); err == nil {
			t.Fatal("symlink config accepted")
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		path := writeRuntimeConfig(t, strings.Replace(validRuntimeConfigJSON, `"level": "observe"`, `"level": "observe", "debug": true`, 1), 0o600)
		if _, err := LoadRuntimeConfig(path); err == nil {
			t.Fatal("unknown nested policy field accepted")
		}
	})
	t.Run("trailing JSON", func(t *testing.T) {
		path := writeRuntimeConfig(t, validRuntimeConfigJSON+` {}`, 0o600)
		if _, err := LoadRuntimeConfig(path); err == nil {
			t.Fatal("multiple JSON values accepted")
		}
	})
}

func TestLoadRuntimeConfigAppliesContributionDefaultsWhenEnabled(t *testing.T) {
	withContribution := strings.Replace(validRuntimeConfigJSON,
		`"embeddings":`,
		`"contribution": {"enabled": true, "dataset_repo": "owner/dataset"}, "embeddings":`, 1)
	config, err := LoadRuntimeConfig(writeRuntimeConfig(t, withContribution, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Contribution.Enabled || config.Contribution.DatasetRepo != "owner/dataset" {
		t.Fatalf("contribution settings not parsed: %#v", config.Contribution)
	}
	if config.Contribution.TokenPath != "/etc/nostrhost-agent/hf_token" || config.Contribution.BaseRevision != "main" ||
		config.Contribution.StatePath != "/var/lib/nostrhost-agent/contribution-submitted.jsonl" {
		t.Fatalf("contribution defaults were not applied: %#v", config.Contribution)
	}
}

func TestLoadRuntimeConfigRejectsEnabledContributionWithBadRepo(t *testing.T) {
	badRepo := strings.Replace(validRuntimeConfigJSON,
		`"embeddings":`,
		`"contribution": {"enabled": true, "dataset_repo": "not-a-valid-repo"}, "embeddings":`, 1)
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, badRepo, 0o600)); err == nil {
		t.Fatal("enabled contribution with a malformed dataset repo was accepted")
	}
}

func TestLoadRuntimeConfigRejectsInvalidOrOversizedDurationsAndFiles(t *testing.T) {
	invalidDuration := strings.Replace(validRuntimeConfigJSON, `"6h"`, `"six hours"`, 1)
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, invalidDuration, 0o600)); err == nil {
		t.Fatal("invalid interval accepted")
	}
	negativeTimeout := strings.Replace(validRuntimeConfigJSON, `"90s"`, `"-1s"`, 1)
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, negativeTimeout, 0o600)); err == nil {
		t.Fatal("negative relay timeout accepted")
	}
	partialEmbeddings := strings.Replace(validRuntimeConfigJSON, `"base_url": "http://127.0.0.1:8081/v1", `, "", 1)
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, partialEmbeddings, 0o600)); err == nil {
		t.Fatal("incomplete semantic embedding configuration accepted")
	}
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, strings.Repeat("x", maxRuntimeConfigBytes+1), 0o600)); err == nil {
		t.Fatal("oversized config accepted")
	}
}
