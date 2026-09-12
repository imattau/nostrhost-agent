//go:build !linux

package agent

import "errors"

func ProbeHostCapabilities(string) (HostCapabilities, error) {
	return HostCapabilities{}, errors.New("host model profiling currently supports Linux only")
}

func diskFree(string) (uint64, error) {
	return 0, errors.New("model downloads currently support Linux only")
}
