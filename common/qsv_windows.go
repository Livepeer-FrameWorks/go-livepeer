package common

import (
	"errors"
	"regexp"
	"strconv"
)

func detectQSVDevices() ([]string, error) {
	re := regexp.MustCompile("(?i)intel")
	intelCount := 0

	cards, err := getGPU()
	if err != nil {
		return nil, err
	}

	if len(cards) != 0 {
		for _, card := range cards {
			if card.DeviceInfo != nil && re.MatchString(card.DeviceInfo.Vendor.Name) {
				intelCount++
			}
		}
	} else {
		pci, err := getPCI()
		if err != nil {
			return nil, err
		}
		rePCI := regexp.MustCompile("(?i)display ?controller")
		for _, device := range pci {
			if device.Vendor != nil && re.MatchString(device.Vendor.Name) && rePCI.MatchString(device.Class.Name) {
				intelCount++
			}
		}
	}

	if intelCount == 0 {
		return nil, errors.New("no Intel GPU devices found")
	}

	devices := make([]string, 0, intelCount)
	for i := 0; i < intelCount; i++ {
		devices = append(devices, strconv.Itoa(i))
	}
	return devices, nil
}
