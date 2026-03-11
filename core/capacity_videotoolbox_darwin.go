//go:build darwin

package core

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/IOKitLib.h>
#include <CoreFoundation/CoreFoundation.h>

// Returns GPU core utilization percentage (0-100) from IOAccelerator.
// Note: On Apple Silicon, this measures GPU core utilization, NOT the dedicated
// media engine (video encoder/decoder). The media engine runs independently.
// This metric still catches GPU-side scaling/color conversion work.
static double vtGetGPUUtil() {
	CFMutableDictionaryRef matching = IOServiceMatching("IOAccelerator");
	if (!matching) return -1.0;

	io_iterator_t iter;
	// Use 0 directly instead of kIOMainPortDefault/kIOMasterPortDefault to avoid deprecation warnings
	kern_return_t kr = IOServiceGetMatchingServices(0, matching, &iter);
	if (kr != KERN_SUCCESS) return -1.0;

	double utilization = -1.0;
	io_service_t service;
	while ((service = IOIteratorNext(iter)) != IO_OBJECT_NULL) {
		CFMutableDictionaryRef props = NULL;
		kr = IORegistryEntryCreateCFProperties(service, &props, kCFAllocatorDefault, 0);
		if (kr == KERN_SUCCESS && props) {
			CFDictionaryRef perfStats = (CFDictionaryRef)CFDictionaryGetValue(
				props, CFSTR("PerformanceStatistics"));
			if (perfStats) {
				CFNumberRef util = (CFNumberRef)CFDictionaryGetValue(
					perfStats, CFSTR("Device Utilization %"));
				if (util) {
					CFNumberGetValue(util, kCFNumberDoubleType, &utilization);
				}
			}
			CFRelease(props);
		}
		IOObjectRelease(service);
		if (utilization >= 0) break;
	}
	IOObjectRelease(iter);
	return utilization;
}

// Returns GPU memory in use (bytes) from IOAccelerator.
// On Apple Silicon, this is unified memory allocated by the GPU.
static long long vtGetGPUMemInUse() {
	CFMutableDictionaryRef matching = IOServiceMatching("IOAccelerator");
	if (!matching) return -1;

	io_iterator_t iter;
	kern_return_t kr = IOServiceGetMatchingServices(0, matching, &iter);
	if (kr != KERN_SUCCESS) return -1;

	long long memInUse = -1;
	io_service_t service;
	while ((service = IOIteratorNext(iter)) != IO_OBJECT_NULL) {
		CFMutableDictionaryRef props = NULL;
		kr = IORegistryEntryCreateCFProperties(service, &props, kCFAllocatorDefault, 0);
		if (kr == KERN_SUCCESS && props) {
			CFDictionaryRef perfStats = (CFDictionaryRef)CFDictionaryGetValue(
				props, CFSTR("PerformanceStatistics"));
			if (perfStats) {
				CFNumberRef mem = (CFNumberRef)CFDictionaryGetValue(
					perfStats, CFSTR("In use system memory"));
				if (mem) {
					CFNumberGetValue(mem, kCFNumberLongLongType, &memInUse);
				}
			}
			CFRelease(props);
		}
		IOObjectRelease(service);
		if (memInUse >= 0) break;
	}
	IOObjectRelease(iter);
	return memInUse;
}
*/
import "C"

import (
	"math"
	"sync"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

// VideotoolboxMonitor tracks Apple GPU utilization via IOKit IOAccelerator.
// On Apple Silicon, the media engine (hardware video encoder/decoder) runs
// independently from GPU cores. IOAccelerator reports GPU core utilization
// which captures scaling and color conversion. The EMA of realtime ratio
// is the primary overload signal for VideoToolbox; this provides supplementary data.
type VideotoolboxMonitor struct {
	mu       sync.RWMutex
	gpuUtil  float64
	memAvail uint64
	done     chan struct{}
}

// NewVideotoolboxMonitor creates a VideoToolbox monitor for macOS.
func NewVideotoolboxMonitor(_ string) (HWMonitor, error) {
	return &VideotoolboxMonitor{
		done:     make(chan struct{}),
		memAvail: math.MaxUint64,
	}, nil
}

func (m *VideotoolboxMonitor) EncoderUtil() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.gpuUtil
}

// DecoderUtil returns the same as EncoderUtil — IOAccelerator doesn't distinguish.
func (m *VideotoolboxMonitor) DecoderUtil() float64 { return m.EncoderUtil() }

func (m *VideotoolboxMonitor) MemoryAvailable() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.memAvail
}

func (m *VideotoolboxMonitor) ActiveSessions() int { return 0 }
func (m *VideotoolboxMonitor) MaxHWSessions() int  { return math.MaxInt }

func (m *VideotoolboxMonitor) Start() error {
	go m.pollLoop()
	return nil
}

func (m *VideotoolboxMonitor) Stop() {
	close(m.done)
}

func (m *VideotoolboxMonitor) pollLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.sample()
		}
	}
}

func (m *VideotoolboxMonitor) sample() {
	gpuUtilPct := float64(C.vtGetGPUUtil())
	memInUse := int64(C.vtGetGPUMemInUse())

	var util float64
	if gpuUtilPct >= 0 {
		util = gpuUtilPct / 100.0
	}

	// For memory, use system available memory from the CPU monitor's perspective.
	// Apple Silicon uses unified memory — GPU memory pressure = system memory pressure.
	// Report MaxUint64 to let the CPU monitor (if also running) or the EMA handle this.
	var memAvail uint64 = math.MaxUint64
	if memInUse > 0 {
		sysAvail := darwinReadMemAvail()
		if sysAvail < math.MaxUint64 {
			memAvail = sysAvail
		}
	}

	m.mu.Lock()
	m.gpuUtil = util
	m.memAvail = memAvail
	m.mu.Unlock()
}

func init() {
	RegisterHWMonitorFactory(ffmpeg.Videotoolbox, NewVideotoolboxMonitor)
}
