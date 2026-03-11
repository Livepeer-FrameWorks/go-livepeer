//go:build linux

package core

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
	"golang.org/x/sys/unix"
)

const (
	// Sysfs paths for Intel GPU PMU
	i915SysfsPath = "/sys/bus/event_source/devices/i915"
	xeSysfsPath   = "/sys/bus/event_source/devices/xe"

	// i915 PMU config encoding (from kernel drivers/gpu/drm/i915/i915_pmu.c):
	//   config = (engine_class << 20) | (engine_instance << 16) | sample_type
	// Engine classes: RENDER=0, COPY=1, VIDEO=2, VIDEO_ENHANCE=3
	// Sample types: BUSY=0, WAIT=1, SEMA=2
	i915EngineClassVideo = 2
	i915SampleBusy       = 0
)

// QSVMonitor tracks Intel GPU video engine utilization via i915/xe PMU counters.
// Uses perf_event_open() to read cumulative engine busy nanoseconds.
// Falls back to StubMonitor if PMU is unavailable (no permissions, no i915/xe driver).
//
// Limitation: The i915 PMU exposes a single system-wide VIDEO engine counter, not per-
// render-node. On multi-Intel-GPU hosts, all QSVMonitor instances report the same
// utilization. This is acceptable for the common single-iGPU case. For multi-GPU Intel
// setups the EMA signal still provides per-device tracking via DeviceID.
type QSVMonitor struct {
	mu        sync.RWMutex
	videoUtil float64
	memAvail  uint64
	perfFd    int
	prevNs    uint64
	prevTime  time.Time
	done      chan struct{}
}

// NewQSVMonitor creates an Intel QSV monitor using i915/xe PMU for the VIDEO engine.
// Requires CAP_PERFMON or perf_event_paranoid <= 2.
func NewQSVMonitor(device string) (HWMonitor, error) {
	pmuType, err := readPMUType()
	if err != nil {
		return nil, fmt.Errorf("Intel GPU PMU not available: %w", err)
	}

	// Config for VIDEO engine class 2, instance 0, BUSY sample
	config := uint64(i915EngineClassVideo)<<20 | uint64(0)<<16 | uint64(i915SampleBusy)

	attr := unix.PerfEventAttr{
		Type:   pmuType,
		Config: config,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
	}

	// pid=-1, cpu=0: system-wide on CPU 0 (GPU PMU is system-wide, cpu is arbitrary)
	fd, err := unix.PerfEventOpen(&attr, -1, 0, -1, 0)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open for Intel VIDEO engine failed (need CAP_PERFMON or perf_event_paranoid<=2): %w", err)
	}

	glog.Infof("Intel GPU PMU opened (type=%d, config=0x%x) for QSV monitoring", pmuType, config)

	return &QSVMonitor{
		perfFd:   fd,
		prevTime: time.Now(),
		memAvail: math.MaxUint64,
		done:     make(chan struct{}),
	}, nil
}

// readPMUType reads the PMU type integer from sysfs. Tries i915 first, then xe (Meteor Lake+).
func readPMUType() (uint32, error) {
	for _, path := range []string{i915SysfsPath, xeSysfsPath} {
		typeFile := filepath.Join(path, "type")
		data, err := os.ReadFile(typeFile)
		if err != nil {
			continue
		}
		t, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
		if err != nil {
			continue
		}
		return uint32(t), nil
	}
	return 0, fmt.Errorf("neither i915 nor xe PMU found in sysfs")
}

func (m *QSVMonitor) EncoderUtil() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.videoUtil
}

// DecoderUtil returns the same as EncoderUtil — the i915 VIDEO engine handles both.
func (m *QSVMonitor) DecoderUtil() float64 { return m.EncoderUtil() }

func (m *QSVMonitor) MemoryAvailable() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.memAvail
}

func (m *QSVMonitor) ActiveSessions() int { return 0 }
func (m *QSVMonitor) MaxHWSessions() int  { return math.MaxInt }

func (m *QSVMonitor) Start() error {
	// Take initial reading
	ns, err := m.readCounter()
	if err != nil {
		return fmt.Errorf("initial PMU read failed: %w", err)
	}
	m.prevNs = ns
	m.prevTime = time.Now()

	go m.pollLoop()
	return nil
}

func (m *QSVMonitor) Stop() {
	close(m.done)
	unix.Close(m.perfFd)
}

func (m *QSVMonitor) pollLoop() {
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

func (m *QSVMonitor) sample() {
	ns, err := m.readCounter()
	if err != nil {
		return
	}
	now := time.Now()

	deltaNs := ns - m.prevNs
	deltaWall := now.Sub(m.prevTime)

	var util float64
	if deltaWall > 0 {
		util = float64(deltaNs) / float64(deltaWall.Nanoseconds())
		if util > 1.0 {
			util = 1.0
		}
	}

	// Intel iGPU shares system RAM — use /proc/meminfo
	memAvail := readMemAvailable()

	m.mu.Lock()
	m.videoUtil = util
	m.memAvail = memAvail
	m.mu.Unlock()

	m.prevNs = ns
	m.prevTime = now
}

// readCounter reads the cumulative busy nanoseconds from the perf event fd.
func (m *QSVMonitor) readCounter() (uint64, error) {
	buf := make([]byte, 8)
	_, err := unix.Read(m.perfFd, buf)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf), nil
}

func init() {
	RegisterHWMonitorFactory(ffmpeg.QSV, NewQSVMonitor)
}
