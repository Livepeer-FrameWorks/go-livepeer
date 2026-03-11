package core

import (
	"math"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"

	lpmon "github.com/livepeer/go-livepeer/monitor"
)

// HWMonitor provides hardware-level utilization metrics for a single device.
// Implementations exist per platform: NvidiaMonitor, QSVMonitor, VideotoolboxMonitor, CPUMonitor, StubMonitor.
type HWMonitor interface {
	EncoderUtil() float64    // 0.0-1.0 — hardware encoder utilization
	DecoderUtil() float64    // 0.0-1.0 — hardware decoder utilization
	MemoryAvailable() uint64 // bytes of free VRAM/RAM
	ActiveSessions() int     // current hardware session count
	MaxHWSessions() int      // hardware session limit (MaxInt if unlimited)
	Start() error            // begin polling loop
	Stop()                   // stop polling loop
}

// DeviceState tracks per-device capacity measurements.
type DeviceState struct {
	realtimeEMA  float64 // EMA of (transcode_duration / segment_duration); 1.0 = realtime
	sampleCount  int     // number of samples in EMA
	lastDecision bool    // hysteresis: true=last decision was accept
}

// CapacityManager makes dynamic accept/reject decisions based on hardware
// utilization and measured transcode throughput. It runs as an additional
// gate before the existing static MaxSessions check.
type CapacityManager struct {
	mu           sync.RWMutex
	devices      map[string]*DeviceState
	monitors     map[string]HWMonitor
	accel        ffmpeg.Acceleration
	acceptThresh float64 // accept new work below this (default 0.70)
	rejectThresh float64 // reject new work above this (default 0.80)
}

const (
	DefaultAcceptThreshold = 0.70
	DefaultRejectThreshold = 0.80
	// Minimum free memory before rejecting (256 MB)
	DefaultMemorySafetyMargin uint64 = 256 * 1024 * 1024
	// EMA smoothing factor: new_ema = (1-alpha)*old + alpha*sample
	emaAlpha = 0.3
)

// NewCapacityManager creates a CapacityManager for the given devices and acceleration type.
// monitorFactory creates an HWMonitor for a given device ID; if nil, StubMonitor is used.
func NewCapacityManager(devices []string, accel ffmpeg.Acceleration, acceptThresh, rejectThresh float64, monitorFactory func(device string) HWMonitor) *CapacityManager {
	if acceptThresh <= 0 {
		acceptThresh = DefaultAcceptThreshold
	}
	if rejectThresh <= 0 {
		rejectThresh = DefaultRejectThreshold
	}
	if rejectThresh <= acceptThresh {
		rejectThresh = acceptThresh + 0.10
	}

	cm := &CapacityManager{
		devices:      make(map[string]*DeviceState, len(devices)),
		monitors:     make(map[string]HWMonitor, len(devices)),
		accel:        accel,
		acceptThresh: acceptThresh,
		rejectThresh: rejectThresh,
	}

	for _, dev := range devices {
		cm.devices[dev] = &DeviceState{lastDecision: true} // start optimistic
		if monitorFactory != nil {
			cm.monitors[dev] = monitorFactory(dev)
		} else {
			cm.monitors[dev] = &StubMonitor{}
		}
	}

	return cm
}

// Start begins all hardware monitor polling loops.
func (cm *CapacityManager) Start() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for dev, mon := range cm.monitors {
		if err := mon.Start(); err != nil {
			glog.Warningf("CapacityManager: failed to start monitor for device %s: %v, using stub", dev, err)
			cm.monitors[dev] = &StubMonitor{}
		}
	}
}

// Stop shuts down all hardware monitor polling loops.
func (cm *CapacityManager) Stop() {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, mon := range cm.monitors {
		mon.Stop()
	}
}

// CheckCapacity returns nil if any device can accept new work, or ErrOrchCap if all are overloaded.
// Two independent signals can veto: hardware utilization and realtime ratio EMA.
// Either signal exceeding rejectThresh causes rejection for that device.
// Accept only if the worst of all signals is below thresholds.
func (cm *CapacityManager) CheckCapacity() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for deviceID, state := range cm.devices {
		mon := cm.monitors[deviceID]

		// Hard gate: memory
		if mon.MemoryAvailable() < DefaultMemorySafetyMargin {
			continue
		}

		// Signal 1: Hardware counters — "is the hardware overloaded right now?"
		encUtil := mon.EncoderUtil()
		decUtil := mon.DecoderUtil()
		hwUtil := math.Max(encUtil, decUtil)

		// Signal 2: Realtime ratio EMA — "are we keeping up with realtime?"
		emaUtil := state.realtimeEMA

		// Worst of all signals
		worst := math.Max(hwUtil, emaUtil)

		// Report metrics
		if lpmon.Enabled {
			lpmon.CapacityHWEncoder(encUtil)
			lpmon.CapacityHWDecoder(decUtil)
			lpmon.CapacityRealtimeEMA(emaUtil)
			lpmon.CapacityUtilization(worst)
		}

		// Hysteresis deadband
		if worst < cm.acceptThresh {
			state.lastDecision = true
			if lpmon.Enabled {
				lpmon.CapacityAccepted()
			}
			return nil // ACCEPT — clearly has headroom
		} else if worst > cm.rejectThresh {
			state.lastDecision = false
			continue // REJECT — protect existing streams
		} else {
			// In deadband: hold previous decision to prevent flapping
			if state.lastDecision {
				if lpmon.Enabled {
					lpmon.CapacityAccepted()
				}
				return nil // ACCEPT — was accepting, avoid flapping
			}
			continue // REJECT — was rejecting, stay conservative
		}
	}

	if lpmon.Enabled {
		lpmon.CapacityRejected()
	}
	return ErrOrchCap
}

// RecordResult updates the realtime ratio EMA for a device after a transcode completes.
// Unknown deviceIDs are silently ignored to prevent phantom device creation.
func (cm *CapacityManager) RecordResult(deviceID string, transcodeDuration, segmentDuration time.Duration) {
	if segmentDuration <= 0 {
		return
	}
	ratio := transcodeDuration.Seconds() / segmentDuration.Seconds()

	cm.mu.Lock()
	defer cm.mu.Unlock()

	state, ok := cm.devices[deviceID]
	if !ok {
		glog.V(6).Infof("CapacityManager: ignoring RecordResult for unknown device %s", deviceID)
		return
	}

	if state.sampleCount == 0 {
		state.realtimeEMA = ratio
	} else {
		state.realtimeEMA = (1-emaAlpha)*state.realtimeEMA + emaAlpha*ratio
	}
	state.sampleCount++
}

// SeedBaseline sets an initial realtime ratio for a device from boot calibration.
// Unknown deviceIDs are silently ignored.
func (cm *CapacityManager) SeedBaseline(deviceID string, ratio float64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	state, ok := cm.devices[deviceID]
	if !ok {
		glog.V(6).Infof("CapacityManager: ignoring SeedBaseline for unknown device %s", deviceID)
		return
	}
	state.realtimeEMA = ratio
	state.sampleCount = 1
}

// Utilization returns the utilization of the least-loaded device (0.0-1.0).
// Useful for metrics and logging.
func (cm *CapacityManager) Utilization() float64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if len(cm.devices) == 0 {
		return 0.0
	}

	// Report the utilization of the least-loaded device (since that's what a new session would use)
	minUtil := 1.0
	for deviceID, state := range cm.devices {
		mon := cm.monitors[deviceID]
		hwUtil := math.Max(mon.EncoderUtil(), mon.DecoderUtil())
		worst := math.Max(hwUtil, state.realtimeEMA)
		if worst < minUtil {
			minUtil = worst
		}
	}
	return minUtil
}

// ActiveSessions returns the total active session count across all devices.
func (cm *CapacityManager) ActiveSessions() int {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	total := 0
	for _, mon := range cm.monitors {
		total += mon.ActiveSessions()
	}
	return total
}

// AddDevice registers a new device at runtime (e.g., when a remote transcoder connects).
func (cm *CapacityManager) AddDevice(deviceID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if _, exists := cm.devices[deviceID]; exists {
		return
	}
	cm.devices[deviceID] = &DeviceState{lastDecision: true}
	cm.monitors[deviceID] = &StubMonitor{}
}

// RemoveDevice unregisters a device (e.g., when a remote transcoder disconnects).
func (cm *CapacityManager) RemoveDevice(deviceID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if mon, ok := cm.monitors[deviceID]; ok {
		mon.Stop()
		delete(cm.monitors, deviceID)
	}
	delete(cm.devices, deviceID)
}

// DeviceUtilization returns the utilization of a specific device (0.0-1.0).
// Returns 0 if the device is unknown. Used by LoadBalancingTranscoder for
// utilization-aware GPU selection.
func (cm *CapacityManager) DeviceUtilization(deviceID string) float64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	state, ok := cm.devices[deviceID]
	if !ok {
		return 0
	}
	mon := cm.monitors[deviceID]
	hwUtil := math.Max(mon.EncoderUtil(), mon.DecoderUtil())
	return math.Max(hwUtil, state.realtimeEMA)
}

// StubMonitor returns permissive defaults — effectively disables hardware monitoring
// and lets the static MaxSessions backstop and EMA do the work.
type StubMonitor struct{}

func (s *StubMonitor) EncoderUtil() float64    { return 0.0 }
func (s *StubMonitor) DecoderUtil() float64    { return 0.0 }
func (s *StubMonitor) MemoryAvailable() uint64 { return math.MaxUint64 }
func (s *StubMonitor) ActiveSessions() int     { return 0 }
func (s *StubMonitor) MaxHWSessions() int      { return math.MaxInt }
func (s *StubMonitor) Start() error            { return nil }
func (s *StubMonitor) Stop()                   {}

// Monitor factory registry — platform-specific init() functions register factories
// for their acceleration type. This allows automatic selection of the right monitor
// implementation based on which hardware acceleration is active.
var (
	hwMonitorRegistryMu sync.RWMutex
	hwMonitorRegistry   = make(map[ffmpeg.Acceleration]func(string) (HWMonitor, error))
)

// RegisterHWMonitorFactory registers a factory for creating HWMonitors for a given acceleration type.
// Called from platform-specific init() functions (e.g., capacity_nvidia_linux.go, capacity_cpu_darwin.go).
func RegisterHWMonitorFactory(accel ffmpeg.Acceleration, factory func(string) (HWMonitor, error)) {
	hwMonitorRegistryMu.Lock()
	defer hwMonitorRegistryMu.Unlock()
	hwMonitorRegistry[accel] = factory
}

// GetHWMonitorFactory returns a monitor factory for the given acceleration type.
// If no factory is registered (e.g., unsupported platform), returns nil — NewCapacityManager
// will fall back to StubMonitor. Factory errors are caught and logged per device.
func GetHWMonitorFactory(accel ffmpeg.Acceleration) func(string) HWMonitor {
	hwMonitorRegistryMu.RLock()
	factory, ok := hwMonitorRegistry[accel]
	hwMonitorRegistryMu.RUnlock()
	if !ok {
		return nil
	}
	return func(device string) HWMonitor {
		mon, err := factory(device)
		if err != nil {
			glog.Warningf("HW monitor creation failed for device %s: %v, using stub", device, err)
			return &StubMonitor{}
		}
		return mon
	}
}
