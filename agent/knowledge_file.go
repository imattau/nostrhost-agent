package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxKnowledgeCorpusBytes = 32 * 1024 * 1024

// LoadKnowledgeDocuments reads a bounded JSON corpus for a local agent. It
// rejects symlinks and non-regular files so a configured corpus path cannot
// silently redirect retrieval to an unexpected source.
func LoadKnowledgeDocuments(path string) ([]KnowledgeDocument, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect knowledge corpus: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxKnowledgeCorpusBytes {
		return nil, errors.New("knowledge corpus must be a regular file no larger than 32 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open knowledge corpus: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, errors.New("knowledge corpus changed while it was being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxKnowledgeCorpusBytes+1))
	if err != nil || len(data) > maxKnowledgeCorpusBytes {
		return nil, errors.New("knowledge corpus exceeds the 32 MiB size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var documents []KnowledgeDocument
	if err := decoder.Decode(&documents); err != nil {
		return nil, errors.New("knowledge corpus contains invalid JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("knowledge corpus must contain one JSON value")
	}
	if _, err := NewLocalRetriever(documents); err != nil {
		return nil, fmt.Errorf("invalid knowledge corpus: %w", err)
	}
	return documents, nil
}

// SaveKnowledgeDocuments atomically replaces the local JSON corpus with
// owner-selected content. The file is created with mode 0600.
func SaveKnowledgeDocuments(path string, documents []KnowledgeDocument) error {
	if _, err := NewLocalRetriever(documents); err != nil {
		return fmt.Errorf("invalid knowledge corpus: %w", err)
	}
	data, err := json.MarshalIndent(documents, "", "  ")
	if err != nil {
		return errors.New("could not encode knowledge corpus")
	}
	if len(data) > maxKnowledgeCorpusBytes {
		return errors.New("knowledge corpus exceeds the 32 MiB size limit")
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".nostrhost-agent-knowledge-*")
	if err != nil {
		return fmt.Errorf("create temporary knowledge corpus: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("set knowledge corpus permissions: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write knowledge corpus: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync knowledge corpus: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close knowledge corpus: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace knowledge corpus: %w", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open knowledge corpus directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync knowledge corpus directory: %w", err)
	}
	return nil
}
