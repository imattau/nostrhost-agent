package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

const maxAuditRecordBytes = 1 << 20

const (
	maxRecentAuditRecords = 10000
	maxRecentAuditBytes   = 32 * 1024 * 1024
)

// JSONLAuditSink appends durable snapshots to a private local journal. Each
// Save is a checkpoint; Records returns the latest snapshot for each cycle.
type JSONLAuditSink struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
	broken bool
}

func OpenJSONLAuditSink(path string) (*JSONLAuditSink, error) {
	before, statErr := os.Lstat(path)
	if statErr == nil && (before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular()) {
		return nil, errors.New("audit journal must be a regular file, not a symlink")
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect audit journal: %w", statErr)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit journal: %w", err)
	}
	closeOnError := func(cause error) (*JSONLAuditSink, error) {
		file.Close()
		return nil, cause
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() {
		return closeOnError(errors.New("audit journal must be a regular file"))
	}
	if statErr == nil && !os.SameFile(before, after) {
		return closeOnError(errors.New("audit journal changed while it was being opened"))
	}
	if statErr != nil {
		current, err := os.Lstat(path)
		if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, after) {
			return closeOnError(errors.New("audit journal changed while it was being created"))
		}
	}
	if err := file.Chmod(0o600); err != nil {
		return closeOnError(fmt.Errorf("set audit journal permissions: %w", err))
	}
	if err := truncateIncompleteAuditTail(file); err != nil {
		return closeOnError(fmt.Errorf("recover audit journal: %w", err))
	}
	if err := file.Sync(); err != nil {
		return closeOnError(fmt.Errorf("sync recovered audit journal: %w", err))
	}
	sink := &JSONLAuditSink{file: file}
	if _, err := sink.Records(context.Background()); err != nil {
		return closeOnError(fmt.Errorf("validate audit journal: %w", err))
	}
	return sink, nil
}

func (s *JSONLAuditSink) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.file.Sync(); err != nil {
		s.file.Close()
		return err
	}
	return s.file.Close()
}

func (s *JSONLAuditSink) Save(ctx context.Context, trace CycleTrace) error {
	if s == nil || s.file == nil {
		return errors.New("audit journal is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if trace.ID == "" {
		return errors.New("audit trace id is required")
	}
	safe, err := sanitizedTraceCopy(trace)
	if err != nil {
		return fmt.Errorf("sanitize audit trace: %w", err)
	}
	encoded, err := json.Marshal(safe)
	if err != nil || len(encoded) > maxAuditRecordBytes {
		return errors.New("audit record is invalid or exceeds the 1 MiB limit")
	}
	encoded = append(encoded, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.broken {
		return errors.New("audit journal is closed or has an earlier write failure")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.file.Write(encoded); err != nil {
		s.broken = true
		return fmt.Errorf("append audit checkpoint: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		s.broken = true
		return fmt.Errorf("sync audit checkpoint: %w", err)
	}
	return nil
}

func (s *JSONLAuditSink) Records(ctx context.Context) ([]CycleTrace, error) {
	if s == nil || s.file == nil {
		return nil, errors.New("audit journal is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("audit journal is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	latest := make(map[string]CycleTrace)
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 64*1024), maxAuditRecordBytes+1)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var trace CycleTrace
		if err := json.Unmarshal(scanner.Bytes(), &trace); err != nil || trace.ID == "" {
			return nil, errors.New("audit journal contains an invalid record")
		}
		latest[trace.ID] = trace
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read audit journal: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	records := make([]CycleTrace, 0, len(latest))
	for _, trace := range latest {
		records = append(records, trace)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}

// RecentRecords returns the latest snapshot for up to limit cycles by scanning
// a bounded tail of the journal. It is intended for retrieval and other
// bounded consumers; Records remains available for complete export.
func (s *JSONLAuditSink) RecentRecords(ctx context.Context, limit int) ([]CycleTrace, error) {
	if s == nil || s.file == nil {
		return nil, errors.New("audit journal is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("recent audit read requires a context")
	}
	if limit <= 0 || limit > maxRecentAuditRecords {
		limit = maxRecentAuditRecords
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("audit journal is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := s.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect audit journal: %w", err)
	}
	readSize := min(info.Size(), int64(maxRecentAuditBytes))
	if readSize == 0 {
		return nil, nil
	}
	offset := info.Size() - readSize
	data := make([]byte, int(readSize))
	if _, err := s.file.ReadAt(data, offset); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read recent audit history: %w", err)
	}
	if offset > 0 {
		firstNewline := bytes.IndexByte(data, '\n')
		if firstNewline < 0 {
			return nil, nil
		}
		data = data[firstNewline+1:]
	}
	seen := make(map[string]struct{}, limit)
	records := make([]CycleTrace, 0, limit)
	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0 && len(records) < limit; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(lines[i]) == 0 {
			continue
		}
		var trace CycleTrace
		if err := json.Unmarshal(lines[i], &trace); err != nil || trace.ID == "" {
			return nil, errors.New("audit journal contains an invalid recent record")
		}
		if _, exists := seen[trace.ID]; exists {
			continue
		}
		seen[trace.ID] = struct{}{}
		records = append(records, trace)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}

func sanitizedTraceCopy(trace CycleTrace) (CycleTrace, error) {
	encoded, err := json.Marshal(trace)
	if err != nil {
		return CycleTrace{}, err
	}
	var copy CycleTrace
	if err := json.Unmarshal(encoded, &copy); err != nil {
		return CycleTrace{}, err
	}
	copy.Observations = sanitizeMap(copy.Observations, nil)
	for i := range copy.Proposals {
		copy.Proposals[i].Proposal.Args = sanitizeMap(copy.Proposals[i].Proposal.Args, nil)
		copy.Proposals[i].Result = sanitizeMap(copy.Proposals[i].Result, nil)
	}
	return copy, nil
}

func truncateIncompleteAuditTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	const chunkSize = 4096
	position := info.Size()
	buffer := make([]byte, chunkSize)
	for position > 0 {
		readSize := int64(len(buffer))
		if position < readSize {
			readSize = position
		}
		position -= readSize
		chunk := buffer[:readSize]
		if _, err := file.ReadAt(chunk, position); err != nil && err != io.EOF {
			return err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				return file.Truncate(position + int64(i) + 1)
			}
		}
	}
	return file.Truncate(0)
}
