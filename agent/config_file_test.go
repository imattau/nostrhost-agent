package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validRuntimeConfigJSON = `{
  "relay": {
    "relay_url": "ws://127.0.0.1:4848",
    "agent_secret_key": "1111111111111111111111111111111111111111111111111111111111111111",
    "trusted_server_key": "2222222222222222222222222222222222222222222222222222222222222222",
    "result_timeout": "90s"
  },
  "policy": {"level": "observe", "capabilities": {"health.read": true}},
  "observation_queries": [{"operation": "system.health"}],
  "audit_path": "/var/lib/nostrhost-agent/audit.jsonl",
  "interval": "6h"
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
	if config.Interval != 6*time.Hour || config.Relay.ResultTimeout != 90*time.Second {
		t.Fatalf("runtime durations were not parsed: interval=%s timeout=%s", config.Interval, config.Relay.ResultTimeout)
	}
	if config.Policy.Level != Observe || len(config.ObservationQueries) != 1 || config.Inference.Model != "" {
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

func TestLoadRuntimeConfigRejectsInvalidOrOversizedDurationsAndFiles(t *testing.T) {
	invalidDuration := strings.Replace(validRuntimeConfigJSON, `"6h"`, `"six hours"`, 1)
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, invalidDuration, 0o600)); err == nil {
		t.Fatal("invalid interval accepted")
	}
	negativeTimeout := strings.Replace(validRuntimeConfigJSON, `"90s"`, `"-1s"`, 1)
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, negativeTimeout, 0o600)); err == nil {
		t.Fatal("negative relay timeout accepted")
	}
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, strings.Repeat("x", maxRuntimeConfigBytes+1), 0o600)); err == nil {
		t.Fatal("oversized config accepted")
	}
}
