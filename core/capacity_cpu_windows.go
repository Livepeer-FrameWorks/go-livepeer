//go:build windows

package core

import (
	"math"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/livepeer/lpms/ffmpeg"
)

// CPUMonitor tracks CPU utilization and available memory on Windows
// via kernel32.dll GetSystemTimes and GlobalMemoryStatusEx.
type CPUMonitor struct {
	mu       sync.RWMutex
	cpuUtil  float64
	memAvail uint64
	done     chan struct{}
}

// NewCPUMonitor creates a CPU monitor for software transcoding on Windows.
func NewCPUMonitor(_ string) (HWMonitor, error) {
	return &CPUMonitor{
		done:     make(chan struct{}),
		memAvail: math.MaxUint64,
	}, nil
}

func (m *CPUMonitor) EncoderUtil() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cpuUtil
}

func (m *CPUMonitor) DecoderUtil() float64 { return m.EncoderUtil() }

func (m *CPUMonitor) MemoryAvailable() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.memAvail
}

func (m *CPUMonitor) ActiveSessions() int { return 0 }
func (m *CPUMonitor) MaxHWSessions() int  { return math.MaxInt }

func (m *CPUMonitor) Start() error {
	go m.pollLoop()
	return nil
}

func (m *CPUMonitor) Stop() {
	close(m.done)
}

type cpuTimes struct {
	idle, kernel, user uint64
}

func (t cpuTimes) total() uint64 { return t.kernel + t.user }
func (t cpuTimes) busy() uint64  { return t.kernel + t.user - t.idle }

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes    = kernel32.NewProc("GetSystemTimes")
	procGlobalMemStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

func (m *CPUMonitor) pollLoop() {
	prev := readCPUTimes()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			curr := readCPUTimes()
			totalDelta := curr.total() - prev.total()
			busyDelta := curr.busy() - prev.busy()
			var util float64
			if totalDelta > 0 {
				util = float64(busyDelta) / float64(totalDelta)
			}
			memAvail := readWinMemAvailable()

			m.mu.Lock()
			m.cpuUtil = util
			m.memAvail = memAvail
			m.mu.Unlock()

			prev = curr
		}
	}
}

func readCPUTimes() cpuTimes {
	var idle, kernel, user syscall.Filetime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return cpuTimes{}
	}
	return cpuTimes{
		idle:   fileTimeToUint64(idle),
		kernel: fileTimeToUint64(kernel),
		user:   fileTimeToUint64(user),
	}
}

func fileTimeToUint64(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

func readWinMemAvailable() uint64 {
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	ret, _, _ := procGlobalMemStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if ret == 0 {
		return math.MaxUint64
	}
	return ms.ullAvailPhys
}

func init() {
	RegisterHWMonitorFactory(ffmpeg.Software, NewCPUMonitor)
}
