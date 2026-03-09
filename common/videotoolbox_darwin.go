package common

func detectVideotoolboxDevices() ([]string, error) {
	// VideoToolbox is a macOS system framework — always available, no device path needed.
	// An empty device string passes NULL to av_hwdevice_ctx_create, which is correct for VT.
	return []string{""}, nil
}
