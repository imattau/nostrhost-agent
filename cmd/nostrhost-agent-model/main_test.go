package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func runtimeArchive(t *testing.T, entries []*tar.Header) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "runtime.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, header := range entries {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("runtime")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestExtractRuntimeArchiveMaterializesSafeSonameLink(t *testing.T) {
	archive := runtimeArchive(t, []*tar.Header{
		{Name: "llama/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: 7},
		{Name: "llama/libllama.so.0.4.0", Typeflag: tar.TypeReg, Mode: 0o755, Size: 7},
		{Name: "llama/libllama.so.0", Typeflag: tar.TypeSymlink, Linkname: "libllama.so.0.4.0"},
	})
	destination := filepath.Join(t.TempDir(), "runtime")
	if err := extractRuntimeArchive(archive, destination, "llama-server"); err != nil {
		t.Fatal(err)
	}
	target, err := os.Stat(filepath.Join(destination, "libllama.so.0.4.0"))
	if err != nil {
		t.Fatal(err)
	}
	alias, err := os.Lstat(filepath.Join(destination, "libllama.so.0"))
	if err != nil {
		t.Fatal(err)
	}
	if !alias.Mode().IsRegular() || !os.SameFile(target, alias) {
		t.Fatal("soname alias was not materialized as a hard link")
	}
}

func TestExtractRuntimeArchiveDropsEscapingSymlink(t *testing.T) {
	archive := runtimeArchive(t, []*tar.Header{
		{Name: "llama/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: 7},
		{Name: "llama/escape", Typeflag: tar.TypeSymlink, Linkname: "../../outside"},
	})
	destination := filepath.Join(t.TempDir(), "runtime")
	if err := extractRuntimeArchive(archive, destination, "llama-server"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "escape")); !os.IsNotExist(err) {
		t.Fatalf("escaping link was extracted: %v", err)
	}
}
