//go:build !linux && !windows

package common

import "fmt"

func detectQSVDevices() ([]string, error) {
	return nil, fmt.Errorf("-qsv all is not supported on this platform; specify device paths explicitly")
}
