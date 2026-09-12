//go:build linux

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProbeHostCapabilities collects local Linux resource facts. It does not send
// telemetry; nvidia-smi is queried only when present and with fixed arguments.
func ProbeHostCapabilities(modelDirectory string) (HostCapabilities, error) {
	if runtime.GOOS != "linux" {
		return HostCapabilities{}, errors.New("host model profiling currently supports Linux only")
	}
	profile := HostCapabilities{
		OS: runtime.GOOS, Architecture: runtime.GOARCH,
		LogicalCPUs: runtime.NumCPU(), GPUProbe: "nvidia-smi unavailable; no NVIDIA GPU details collected",
	}
	total, available, err := readMemoryInfo("/proc/meminfo")
	if err != nil {
		return HostCapabilities{}, err
	}
	profile.MemoryTotalBytes, profile.MemoryAvailableBytes = total, available
	profile.CPUFeatures = readCPUFeatures("/proc/cpuinfo")
	free, err := diskFree(modelDirectory)
	if err != nil {
		return HostCapabilities{}, fmt.Errorf("inspect model directory: %w", err)
	}
	profile.ModelDirFreeBytes = free
	profile.NvidiaGPUs, profile.GPUProbe = probeNvidia()
	return profile, nil
}

func readMemoryInfo(path string) (uint64, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read Linux memory information: %w", err)
	}
	defer file.Close()
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[0] != "MemTotal:" && fields[0] != "MemAvailable:" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kilobytes > ^uint64(0)/1024 {
			return 0, 0, errors.New("Linux memory information contains an invalid value")
		}
		values[strings.TrimSuffix(fields[0], ":")] = kilobytes * 1024
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("read Linux memory information: %w", err)
	}
	total, hasTotal := values["MemTotal"]
	available, hasAvailable := values["MemAvailable"]
	if !hasTotal || !hasAvailable || total == 0 {
		return 0, 0, errors.New("Linux memory information is missing MemTotal or MemAvailable")
	}
	return total, available, nil
}

func readCPUFeatures(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	for scanner := bufio.NewScanner(file); scanner.Scan(); {
		line := scanner.Text()
		if !strings.HasPrefix(line, "flags") && !strings.HasPrefix(line, "Features") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		features := strings.Fields(parts[1])
		if len(features) > 256 {
			features = features[:256]
		}
		sort.Strings(features)
		features = uniqueStrings(features)
		return features
	}
	return nil
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func diskFree(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, errors.New("model path must be a real directory, not a symlink")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func probeNvidia() ([]GPUProfile, string) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, "nvidia-smi unavailable; no NVIDIA GPU details collected"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path,
		"--query-gpu=name,memory.total,memory.free",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, "nvidia-smi query failed; GPU compatibility is unknown"
	}
	var gpus []GPUProfile
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 3 {
			continue
		}
		totalMiB, totalErr := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		freeMiB, freeErr := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
		if totalErr != nil || freeErr != nil {
			continue
		}
		gpus = append(gpus, GPUProfile{
			Name:             strings.TrimSpace(fields[0]),
			MemoryTotalBytes: totalMiB * 1024 * 1024,
			MemoryFreeBytes:  freeMiB * 1024 * 1024,
		})
	}
	if len(gpus) == 0 {
		return nil, "nvidia-smi is present but returned no parseable devices"
	}
	return gpus, "NVIDIA devices queried with nvidia-smi; other GPU vendors are not probed"
}
