package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLAuditSinkPersistsLatestPrivateRedactedTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenJSONLAuditSink(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	initial := CycleTrace{ID: "cycle-1", StartedAt: started, Trigger: "health", Observations: map[string]any{"status": "failed"}, Result: "running"}
	if err := sink.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	final := initial
	final.Result = "verified"
	final.Observations = map[string]any{"status": "healthy", "api_token": "must-not-persist"}
	final.Proposals = []ProposalRecord{{
		Proposal: Proposal{Operation: "app.health", Args: map[string]any{"app": "photos", "password": "must-not-persist"}},
		Result:   map[string]any{"status": "healthy", "nested": map[string]any{"secret": "must-not-persist"}},
	}}
	if err := sink.Save(context.Background(), final); err != nil {
		t.Fatal(err)
	}
	records, err := sink.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Result != "verified" {
		t.Fatalf("journal did not return the latest cycle checkpoint: %#v", records)
	}
	if records[0].Observations["api_token"] != redactedValue || records[0].Proposals[0].Proposal.Args["password"] != redactedValue {
		t.Fatalf("sensitive trace values were not redacted: %#v", records[0])
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "must-not-persist") {
		t.Fatal("sensitive value found in persisted journal")
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}
}

func TestJSONLAuditSinkRecoversIncompleteFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenJSONLAuditSink(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Save(context.Background(), CycleTrace{ID: "complete", Result: "observed"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"id":"partial"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sink, err = OpenJSONLAuditSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	if err := sink.Save(context.Background(), CycleTrace{ID: "after-restart", Result: "observed"}); err != nil {
		t.Fatal(err)
	}
	records, err := sink.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("incomplete tail recovery lost complete records: %#v", records)
	}
}

func TestJSONLAuditSinkRejectsSymlinkAndOversizedRecords(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONLAuditSink(link); err == nil {
		t.Fatal("symlink journal accepted")
	}
	sink, err := OpenJSONLAuditSink(filepath.Join(directory, "normal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	if err := sink.Save(context.Background(), CycleTrace{ID: "large", Observations: map[string]any{"detail": strings.Repeat("x", maxAuditRecordBytes)}}); err == nil {
		t.Fatal("oversized audit record accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Save(canceled, CycleTrace{ID: "canceled"}); err == nil {
		t.Fatal("audit save ignored canceled context")
	}
}

func TestJSONLAuditSinkRejectsCorruptCompleteJournalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.jsonl")
	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONLAuditSink(path); err == nil {
		t.Fatal("corrupt complete journal record accepted")
	}
}
