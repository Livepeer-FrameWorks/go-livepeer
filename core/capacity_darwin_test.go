//go:build darwin

package core

import (
	"testing"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCPUMonitor_Darwin(t *testing.T) {
	mon, err := NewCPUMonitor("")
	require.NoError(t, err)
	require.NotNil(t, mon)

	err = mon.Start()
	require.NoError(t, err)
	defer mon.Stop()

	// Wait for at least one sample
	time.Sleep(1500 * time.Millisecond)

	// CPU util should be between 0 and 1
	util := mon.EncoderUtil()
	assert.GreaterOrEqual(t, util, 0.0, "CPU util should be >= 0")
	assert.LessOrEqual(t, util, 1.0, "CPU util should be <= 1")

	// DecoderUtil should equal EncoderUtil for CPU
	assert.Equal(t, util, mon.DecoderUtil())

	// Memory should be a reasonable value
	mem := mon.MemoryAvailable()
	assert.Greater(t, mem, uint64(0), "memory should be > 0")
}

func TestVideotoolboxMonitor_Darwin(t *testing.T) {
	mon, err := NewVideotoolboxMonitor("")
	require.NoError(t, err)
	require.NotNil(t, mon)

	err = mon.Start()
	require.NoError(t, err)
	defer mon.Stop()

	// Wait for at least one sample
	time.Sleep(1500 * time.Millisecond)

	// GPU util should be between 0 and 1
	util := mon.EncoderUtil()
	assert.GreaterOrEqual(t, util, 0.0, "GPU util should be >= 0")
	assert.LessOrEqual(t, util, 1.0, "GPU util should be <= 1")
}

func TestCPUMonitor_RegisteredForSoftware(t *testing.T) {
	factory := GetHWMonitorFactory(ffmpeg.Software)
	assert.NotNil(t, factory, "CPU monitor should be registered for Software accel")

	mon := factory("cpu")
	assert.NotNil(t, mon, "factory should return a monitor")
}

func TestVideotoolboxMonitor_RegisteredForVT(t *testing.T) {
	factory := GetHWMonitorFactory(ffmpeg.Videotoolbox)
	assert.NotNil(t, factory, "VT monitor should be registered for Videotoolbox accel")

	mon := factory("")
	assert.NotNil(t, mon, "factory should return a monitor")
}
