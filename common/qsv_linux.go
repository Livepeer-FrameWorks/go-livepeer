package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func detectQSVDevices() ([]string, error) {
	var devices []string
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return nil, fmt.Errorf("cannot read /sys/class/drm: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "renderD") {
			continue
		}
		vendorPath := filepath.Join("/sys/class/drm", name, "device/vendor")
		data, err := os.ReadFile(vendorPath)
		if err != nil {
			continue
		}
		vendor := strings.TrimSpace(string(data))
		if vendor == "0x8086" {
			devices = append(devices, filepath.Join("/dev/dri", name))
		}
	}
	if len(devices) == 0 {
		return nil, errors.New("no Intel GPU render nodes found")
	}
	return devices, nil
}
