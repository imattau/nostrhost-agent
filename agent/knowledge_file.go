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
	file, _, err := openVerifiedFile(path, WithMaxOpenSize(maxKnowledgeCorpusBytes))
	if err != nil {
		return nil, fmt.Errorf("inspect knowledge corpus: %w", err)
	}
	defer file.Close()
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
	write := func(file *os.File) error {
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("write knowledge corpus: %w", err)
		}
		return nil
	}
	publish := func(tempPath string) error {
		if err := os.Rename(tempPath, path); err != nil {
			return fmt.Errorf("replace knowledge corpus: %w", err)
		}
		return nil
	}
	return writeFileAtomic0600(directory, ".nostrhost-agent-knowledge-*", write, publish)
}
