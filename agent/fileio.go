package agent

import (
	"errors"
	"fmt"
	"os"
)

// openFileOptions configures openVerifiedFile. The zero value performs only
// the checks every call site needs: the path must resolve to a regular
// file, not a symlink, both before and after the open (the TOCTOU guard).
type openFileOptions struct {
	maxSize          int64
	rejectGroupOther bool
}

// OpenFileOption customizes openVerifiedFile for one call site's extra
// requirements. Call sites that don't need a stricter check simply omit the
// corresponding option -- they never lose the shared symlink/TOCTOU checks,
// which always apply.
type OpenFileOption func(*openFileOptions)

// WithMaxOpenSize rejects a file larger than n bytes, checked both before
// the file is opened and again against the file actually opened. Callers
// still bound their own read (for example with io.LimitReader) -- this is
// an early, defense-in-depth reject, not the sole enforcement.
func WithMaxOpenSize(n int64) OpenFileOption {
	return func(o *openFileOptions) { o.maxSize = n }
}

// WithRejectGroupOtherPerms rejects a file that grants any permission to
// group or other (mode&0o077 != 0), checked both before and after the open.
// Use it for files that may hold secrets (tokens, relay keys, runtime
// config).
func WithRejectGroupOtherPerms() OpenFileOption {
	return func(o *openFileOptions) { o.rejectGroupOther = true }
}

// openVerifiedFile opens path for reading only after confirming it is a
// regular file, not a symlink, and re-verifies that (plus any options)
// against the file actually opened -- a TOCTOU guard against the path being
// swapped out between the initial check and the open. It is the shared
// "open a local file safely" sequence used by every call site in this
// package that reads a file from an operator- or config-supplied path.
//
// The returned *os.File is positioned at the start and must be closed by
// the caller. The returned os.FileInfo is the post-open Stat result.
func openVerifiedFile(path string, opts ...OpenFileOption) (*os.File, os.FileInfo, error) {
	var cfg openFileOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect file: %w", err)
	}
	if err := verifyRegularFileMode(before, cfg); err != nil {
		return nil, nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open file: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("inspect opened file: %w", err)
	}
	if !os.SameFile(before, after) {
		file.Close()
		return nil, nil, errors.New("file changed while it was being opened")
	}
	if err := verifyRegularFileMode(after, cfg); err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, after, nil
}

func verifyRegularFileMode(info os.FileInfo, cfg openFileOptions) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("file must be a regular file, not a symlink")
	}
	if cfg.rejectGroupOther && info.Mode().Perm()&0o077 != 0 {
		return errors.New("file has group or other permissions; secure it to mode 0600 first")
	}
	if cfg.maxSize > 0 && info.Size() > cfg.maxSize {
		return errors.New("file exceeds the configured size limit")
	}
	return nil
}

// verifyRealDirectory confirms path is a real directory, not a symlink (a
// symlinked directory could be swapped to redirect a download or export
// outside the directory an operator configured).
func verifyRealDirectory(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("path must be a real directory, not a symlink")
	}
	return info, nil
}

// writeFileAtomic0600 creates a private (mode 0600) temporary file in dir,
// lets write populate it, syncs and closes it, then hands its path to
// publish to move it into place (for example via os.Rename or os.Link).
// Once publish succeeds, dir itself is fsynced so the publish is durable
// across a crash, not just the file's own contents. The temporary file is
// removed on any failure (and is a harmless no-op to remove again if
// publish already consumed it).
func writeFileAtomic0600(dir, pattern string, write func(*os.File) error, publish func(tempPath string) error) error {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)

	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure temporary file: %w", err)
	}
	if err := write(file); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := publish(tempPath); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for durability sync: %w", err)
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

// safeEnum returns value unchanged when it is one of allowed, and
// "unknown" otherwise. It is used everywhere a value coming from a model
// planner's own output, or from an on-disk trace, is about to be embedded
// in an exported contribution -- so an unrecognized value degrades to
// "unknown" instead of passing arbitrary planner-controlled text through.
func safeEnum[T ~string](value T, allowed ...T) string {
	for _, candidate := range allowed {
		if value == candidate {
			return string(value)
		}
	}
	return "unknown"
}
