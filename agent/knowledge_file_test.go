package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestKnowledgeCorpusPersistsAtomicallyWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge.json")
	want := []KnowledgeDocument{{ID: "incident-1", Source: "verified-trace", Text: "Database recovered after storage was remounted."}}
	if err := SaveKnowledgeDocuments(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKnowledgeDocuments(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("knowledge corpus changed across persistence: got %#v want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("knowledge corpus mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadKnowledgeCorpusRejectsSymlinksAndUnknownFields(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKnowledgeDocuments(link); err == nil {
		t.Fatal("symlink corpus accepted")
	}
	if err := os.WriteFile(target, []byte(`[ {"id":"x","source":"docs","text":"ok","extra":true} ]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKnowledgeDocuments(target); err == nil {
		t.Fatal("unknown corpus field accepted")
	}
}
