//go:build darwin

package core

/*
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <mach/processor_info.h>
#include <mach/vm_statistics.h>

// Returns aggregated CPU ticks across all cores.
// out[0]=user, out[1]=system, out[2]=idle, out[3]=nice
int darwinGetCPUTicks(unsigned long long *out) {
	mach_msg_type_number_t count;
	processor_info_array_t cpuInfo;
	natural_t numCPUs;
	kern_return_t ret = host_processor_info(mach_host_self(),
		PROCESSOR_CPU_LOAD_INFO, &numCPUs, &cpuInfo, &count);
	if (ret != KERN_SUCCESS) return -1;

	out[0] = 0; out[1] = 0; out[2] = 0; out[3] = 0;
	for (natural_t i = 0; i < numCPUs; i++) {
		out[0] += cpuInfo[i * CPU_STATE_MAX + CPU_STATE_USER];
		out[1] += cpuInfo[i * CPU_STATE_MAX + CPU_STATE_SYSTEM];
		out[2] += cpuInfo[i * CPU_STATE_MAX + CPU_STATE_IDLE];
		out[3] += cpuInfo[i * CPU_STATE_MAX + CPU_STATE_NICE];
	}

	vm_deallocate(mach_task_self(), (vm_address_t)cpuInfo,
		count * sizeof(integer_t));
	return 0;
}

// Returns available memory (free + inactive pages) in bytes.
unsigned long long darwinGetMemAvail() {
	vm_statistics64_data_t vmStats;
	mach_msg_type_number_t infoCount = HOST_VM_INFO64_COUNT;
	kern_return_t ret = host_statistics64(mach_host_self(), HOST_VM_INFO64,
		(host_info64_t)&vmStats, &infoCount);
	if (ret != KERN_SUCCESS) return 0;

	unsigned long long pageSize = vm_kernel_page_size;
	return (vmStats.free_count + vmStats.inactive_count) * pageSize;
}
*/
import "C"

import (
	"math"
	"sync"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

type cpuTicks struct {
	user, system, idle, nice uint64
}

func (t cpuTicks) total() uint64 { return t.user + t.system + t.idle + t.nice }
func (t cpuTicks) busy() uint64  { return t.user + t.system + t.nice }

// CPUMonitor tracks CPU utilization and available memory on macOS.
type CPUMonitor struct {
	mu       sync.RWMutex
	cpuUtil  float64
	memAvail uint64
	done     chan struct{}
}

// NewCPUMonitor creates a CPU monitor for software transcoding on macOS.
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

func (m *CPUMonitor) pollLoop() {
	prev := darwinReadCPUTicks()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			curr := darwinReadCPUTicks()
			totalDelta := curr.total() - prev.total()
			busyDelta := curr.busy() - prev.busy()
			var util float64
			if totalDelta > 0 {
				util = float64(busyDelta) / float64(totalDelta)
			}
			memAvail := darwinReadMemAvail()

			m.mu.Lock()
			m.cpuUtil = util
			m.memAvail = memAvail
			m.mu.Unlock()

			prev = curr
		}
	}
}

func darwinReadCPUTicks() cpuTicks {
	var out [4]C.ulonglong
	if C.darwinGetCPUTicks(&out[0]) != 0 {
		return cpuTicks{}
	}
	return cpuTicks{
		user:   uint64(out[0]),
		system: uint64(out[1]),
		idle:   uint64(out[2]),
		nice:   uint64(out[3]),
	}
}

func darwinReadMemAvail() uint64 {
	avail := C.darwinGetMemAvail()
	if avail == 0 {
		return math.MaxUint64
	}
	return uint64(avail)
}

func init() {
	RegisterHWMonitorFactory(ffmpeg.Software, NewCPUMonitor)
}
