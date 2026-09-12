//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCPUFeaturesKeepsLateFeaturesAndSorts(t *testing.T) {
	features := make([]string, 0, 140)
	for i := 0; i < 130; i++ {
		features = append(features, fmt.Sprintf("feature%03d", i))
	}
	features = append(features, "avx2", "avx2")
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte("processor : 0\nflags : "+strings.Join(features, " ")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readCPUFeatures(path)
	if !containsString(got, "avx2") {
		t.Fatal("CPU feature beyond the initial flags was lost")
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("CPU features are not sorted: %#v", got)
		}
		if got[i] == got[i-1] {
			t.Fatalf("CPU feature was not de-duplicated: %#v", got)
		}
	}
}

func TestReadMemoryInfoConvertsKiBToBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemTotal: 4096 kB\nMemFree: 100 kB\nMemAvailable: 2048 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	total, available, err := readMemoryInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4*1024*1024 || available != 2*1024*1024 {
		t.Fatalf("memory profile = %d/%d, want %d/%d", total, available, 4*1024*1024, 2*1024*1024)
	}
}
