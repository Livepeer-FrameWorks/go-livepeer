//go:build !darwin

package common

import "fmt"

func detectVideotoolboxDevices() ([]string, error) {
	return nil, fmt.Errorf("-videotoolbox is only supported on macOS")
}
