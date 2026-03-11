package core

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockMonitor struct {
	encoderUtil float64
	decoderUtil float64
	memAvail    uint64
	sessions    int
	maxSessions int
}

func (m *mockMonitor) EncoderUtil() float64    { return m.encoderUtil }
func (m *mockMonitor) DecoderUtil() float64    { return m.decoderUtil }
func (m *mockMonitor) MemoryAvailable() uint64 { return m.memAvail }
func (m *mockMonitor) ActiveSessions() int     { return m.sessions }
func (m *mockMonitor) MaxHWSessions() int      { return m.maxSessions }
func (m *mockMonitor) Start() error            { return nil }
func (m *mockMonitor) Stop()                   {}

func newMockMonitor() *mockMonitor {
	return &mockMonitor{
		memAvail:    4 * 1024 * 1024 * 1024, // 4GB
		maxSessions: math.MaxInt,
	}
}

func TestCapacityManager_AcceptWhenIdle(t *testing.T) {
	mon := newMockMonitor()
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return mon
	})

	err := cm.CheckCapacity()
	assert.NoError(t, err, "should accept when idle")
}

func TestCapacityManager_RejectWhenHWOverloaded(t *testing.T) {
	mon := newMockMonitor()
	mon.encoderUtil = 0.90 // above reject threshold (0.80)
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return mon
	})

	err := cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should reject when encoder util > rejectThresh")
}

func TestCapacityManager_RejectWhenEMAOverloaded(t *testing.T) {
	mon := newMockMonitor()
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return mon
	})

	// Simulate a device that's transcoding at 0.9x realtime
	cm.RecordResult("0", 900*time.Millisecond, 1000*time.Millisecond)

	err := cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should reject when EMA > rejectThresh")
}

func TestCapacityManager_AcceptBelowThreshold(t *testing.T) {
	mon := newMockMonitor()
	mon.encoderUtil = 0.50
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return mon
	})

	// EMA also moderate
	cm.RecordResult("0", 500*time.Millisecond, 1000*time.Millisecond)

	err := cm.CheckCapacity()
	assert.NoError(t, err, "should accept when both signals below acceptThresh")
}

func TestCapacityManager_Hysteresis(t *testing.T) {
	mon := newMockMonitor()
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0.70, 0.80, func(device string) HWMonitor {
		return mon
	})

	// Start in deadband (0.75) — should accept because lastDecision starts true
	mon.encoderUtil = 0.75
	err := cm.CheckCapacity()
	assert.NoError(t, err, "should accept in deadband when last decision was accept")

	// Push above reject threshold
	mon.encoderUtil = 0.85
	err = cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should reject above rejectThresh")

	// Drop back to deadband — should still reject because last decision was reject
	mon.encoderUtil = 0.75
	err = cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should stay rejected in deadband after rejection")

	// Drop below accept threshold — should accept again
	mon.encoderUtil = 0.60
	err = cm.CheckCapacity()
	assert.NoError(t, err, "should accept below acceptThresh")

	// Back to deadband — should now accept because last decision was accept
	mon.encoderUtil = 0.75
	err = cm.CheckCapacity()
	assert.NoError(t, err, "should accept in deadband after acceptance")
}

func TestCapacityManager_MultiDevice(t *testing.T) {
	monitors := map[string]*mockMonitor{
		"0": newMockMonitor(),
		"1": newMockMonitor(),
	}
	monitors["0"].encoderUtil = 0.90 // overloaded
	monitors["1"].encoderUtil = 0.30 // has headroom

	cm := NewCapacityManager([]string{"0", "1"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return monitors[device]
	})

	err := cm.CheckCapacity()
	assert.NoError(t, err, "should accept if any device has capacity")
}

func TestCapacityManager_AllDevicesOverloaded(t *testing.T) {
	monitors := map[string]*mockMonitor{
		"0": newMockMonitor(),
		"1": newMockMonitor(),
	}
	monitors["0"].encoderUtil = 0.90
	monitors["1"].decoderUtil = 0.90

	cm := NewCapacityManager([]string{"0", "1"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return monitors[device]
	})

	err := cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should reject when all devices overloaded")
}

func TestCapacityManager_MemoryGate(t *testing.T) {
	mon := newMockMonitor()
	mon.memAvail = 100 * 1024 * 1024 // 100MB < 256MB safety margin
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return mon
	})

	err := cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should reject when memory below safety margin")
}

func TestCapacityManager_EMAConvergence(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Software, 0, 0, nil)

	// Feed consistent 0.5x realtime ratio
	for i := 0; i < 20; i++ {
		cm.RecordResult("0", 500*time.Millisecond, 1000*time.Millisecond)
	}

	cm.mu.RLock()
	state := cm.devices["0"]
	cm.mu.RUnlock()

	// EMA should converge close to 0.5
	assert.InDelta(t, 0.5, state.realtimeEMA, 0.05, "EMA should converge to 0.5")
	assert.Equal(t, 20, state.sampleCount)
}

func TestCapacityManager_EMAReactsToChange(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Software, 0, 0, nil)

	// Establish baseline at 0.3
	for i := 0; i < 10; i++ {
		cm.RecordResult("0", 300*time.Millisecond, 1000*time.Millisecond)
	}

	// Sudden spike to 0.9
	cm.RecordResult("0", 900*time.Millisecond, 1000*time.Millisecond)

	cm.mu.RLock()
	state := cm.devices["0"]
	cm.mu.RUnlock()

	// EMA should have moved up significantly from the spike
	assert.Greater(t, state.realtimeEMA, 0.4, "EMA should react to spike")
}

func TestCapacityManager_SeedBaseline(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, nil)

	cm.SeedBaseline("0", 0.25)

	cm.mu.RLock()
	state := cm.devices["0"]
	cm.mu.RUnlock()

	assert.Equal(t, 0.25, state.realtimeEMA)
	assert.Equal(t, 1, state.sampleCount)
}

func TestCapacityManager_Utilization(t *testing.T) {
	monitors := map[string]*mockMonitor{
		"0": newMockMonitor(),
		"1": newMockMonitor(),
	}
	monitors["0"].encoderUtil = 0.80
	monitors["1"].encoderUtil = 0.30

	cm := NewCapacityManager([]string{"0", "1"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return monitors[device]
	})

	util := cm.Utilization()
	assert.InDelta(t, 0.30, util, 0.01, "should report least-loaded device util")
}

func TestCapacityManager_NilFactory(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Software, 0, 0, nil)
	require.NotNil(t, cm)

	err := cm.CheckCapacity()
	assert.NoError(t, err, "should accept with StubMonitor (all zeros)")
}

func TestCapacityManager_IgnoreUnknownDevice(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Software, 0, 0, nil)

	// RecordResult for unknown device — should be silently ignored (no phantom device)
	cm.RecordResult("unknown", 500*time.Millisecond, 1000*time.Millisecond)

	cm.mu.RLock()
	_, exists := cm.devices["unknown"]
	_, monExists := cm.monitors["unknown"]
	cm.mu.RUnlock()

	assert.False(t, exists, "should NOT create state for unknown device")
	assert.False(t, monExists, "should NOT create monitor for unknown device")

	// SeedBaseline for unknown device — also ignored
	cm.SeedBaseline("unknown", 0.5)

	cm.mu.RLock()
	_, exists = cm.devices["unknown"]
	cm.mu.RUnlock()

	assert.False(t, exists, "SeedBaseline should NOT create state for unknown device")

	// CheckCapacity still works fine with only the known device
	err := cm.CheckCapacity()
	assert.NoError(t, err)
}

func TestCapacityManager_ZeroDuration(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Software, 0, 0, nil)

	// Should not panic or divide by zero
	cm.RecordResult("0", 500*time.Millisecond, 0)

	cm.mu.RLock()
	state := cm.devices["0"]
	cm.mu.RUnlock()

	assert.Equal(t, 0, state.sampleCount, "should skip zero-duration segments")
}

func TestCapacityManager_EitherSignalCanReject(t *testing.T) {
	mon := newMockMonitor()
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0.70, 0.80, func(device string) HWMonitor {
		return mon
	})

	// HW is fine but EMA says overloaded
	mon.encoderUtil = 0.10
	cm.RecordResult("0", 850*time.Millisecond, 1000*time.Millisecond) // 0.85 > 0.80

	err := cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "EMA alone should be able to reject")

	// Reset: EMA is fine but HW says overloaded
	cm2 := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0.70, 0.80, func(device string) HWMonitor {
		return mon
	})
	cm2.RecordResult("0", 100*time.Millisecond, 1000*time.Millisecond) // 0.10
	mon.encoderUtil = 0.85

	err = cm2.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "HW util alone should be able to reject")
}

func TestStubMonitor(t *testing.T) {
	s := &StubMonitor{}
	assert.Equal(t, 0.0, s.EncoderUtil())
	assert.Equal(t, 0.0, s.DecoderUtil())
	assert.Equal(t, uint64(math.MaxUint64), s.MemoryAvailable())
	assert.Equal(t, 0, s.ActiveSessions())
	assert.Equal(t, math.MaxInt, s.MaxHWSessions())
	assert.NoError(t, s.Start())
	s.Stop() // no-op, just verify no panic
}

func TestCapacityManager_CustomThresholds(t *testing.T) {
	mon := newMockMonitor()
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0.50, 0.60, func(device string) HWMonitor {
		return mon
	})

	// At 0.55 — in deadband, should accept (starts optimistic)
	mon.encoderUtil = 0.55
	err := cm.CheckCapacity()
	assert.NoError(t, err, "should accept in deadband with default optimistic start")

	// At 0.65 — above custom reject threshold
	mon.encoderUtil = 0.65
	err = cm.CheckCapacity()
	assert.Equal(t, ErrOrchCap, err, "should reject above custom rejectThresh")
}

func TestHWMonitorRegistry(t *testing.T) {
	assert := assert.New(t)

	// GetHWMonitorFactory for unregistered accel returns nil
	factory := GetHWMonitorFactory(ffmpeg.Acceleration(255))
	assert.Nil(factory, "unregistered accel should return nil factory")

	// Register a mock factory
	RegisterHWMonitorFactory(ffmpeg.Acceleration(254), func(device string) (HWMonitor, error) {
		return newMockMonitor(), nil
	})

	factory = GetHWMonitorFactory(ffmpeg.Acceleration(254))
	assert.NotNil(factory, "registered accel should return non-nil factory")

	mon := factory("test")
	assert.NotNil(mon, "factory should return a monitor")
	assert.Equal(0.0, mon.EncoderUtil())
}

func TestHWMonitorRegistry_FactoryError(t *testing.T) {
	assert := assert.New(t)

	// Register a factory that returns an error
	RegisterHWMonitorFactory(ffmpeg.Acceleration(253), func(device string) (HWMonitor, error) {
		return nil, fmt.Errorf("test error")
	})

	factory := GetHWMonitorFactory(ffmpeg.Acceleration(253))
	assert.NotNil(factory, "should return wrapper even when factory can error")

	mon := factory("test")
	assert.NotNil(mon, "should fall back to StubMonitor on error")
	// StubMonitor returns 0.0 for utilization
	assert.Equal(0.0, mon.EncoderUtil())
}

func TestCapacityManager_WithRegistryFactory(t *testing.T) {
	assert := assert.New(t)

	// Register a factory that returns a monitor with specific values
	mon := newMockMonitor()
	mon.encoderUtil = 0.50
	RegisterHWMonitorFactory(ffmpeg.Acceleration(252), func(device string) (HWMonitor, error) {
		return mon, nil
	})

	factory := GetHWMonitorFactory(ffmpeg.Acceleration(252))
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Acceleration(252), 0, 0, factory)

	err := cm.CheckCapacity()
	assert.NoError(err, "should accept at 50% util")
}

func TestCapacityManager_AddRemoveDevice(t *testing.T) {
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Software, 0, 0, nil)

	// Add a new device dynamically
	cm.AddDevice("1")

	cm.mu.RLock()
	_, exists := cm.devices["1"]
	_, monExists := cm.monitors["1"]
	cm.mu.RUnlock()

	assert.True(t, exists, "AddDevice should create state")
	assert.True(t, monExists, "AddDevice should create StubMonitor")

	// Adding same device again is a no-op
	cm.AddDevice("1")
	cm.mu.RLock()
	assert.Len(t, cm.devices, 2)
	cm.mu.RUnlock()

	// RecordResult works for dynamically added device
	cm.RecordResult("1", 300*time.Millisecond, 1000*time.Millisecond)
	cm.mu.RLock()
	assert.Equal(t, 1, cm.devices["1"].sampleCount)
	cm.mu.RUnlock()

	// Remove the device
	cm.RemoveDevice("1")

	cm.mu.RLock()
	_, exists = cm.devices["1"]
	_, monExists = cm.monitors["1"]
	cm.mu.RUnlock()

	assert.False(t, exists, "RemoveDevice should delete state")
	assert.False(t, monExists, "RemoveDevice should delete monitor")

	// Removing non-existent device is safe
	cm.RemoveDevice("nonexistent")
}

func TestCapacityManager_DeviceUtilization(t *testing.T) {
	mon := newMockMonitor()
	mon.encoderUtil = 0.40
	cm := NewCapacityManager([]string{"0"}, ffmpeg.Nvidia, 0, 0, func(device string) HWMonitor {
		return mon
	})

	// HW util only (no EMA yet)
	util := cm.DeviceUtilization("0")
	assert.InDelta(t, 0.40, util, 0.01, "should return HW util when no EMA")

	// EMA higher than HW
	cm.RecordResult("0", 700*time.Millisecond, 1000*time.Millisecond)
	util = cm.DeviceUtilization("0")
	assert.InDelta(t, 0.70, util, 0.01, "should return EMA when higher than HW")

	// Unknown device returns 0
	util = cm.DeviceUtilization("unknown")
	assert.Equal(t, 0.0, util, "unknown device should return 0")
}
