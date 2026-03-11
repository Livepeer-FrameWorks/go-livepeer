//go:build linux

package core

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>

// Minimal NVML type definitions (stable ABI, compatible across driver versions)
typedef void* nvmlDevice_t;
typedef int nvmlReturn_t;

typedef struct {
	unsigned long long total;
	unsigned long long free;
	unsigned long long used;
} nvmlMemory_t;

// Function pointers loaded at runtime via dlopen
static void* nvml_lib = NULL;
static nvmlReturn_t (*pNvmlInit)(void) = NULL;
static nvmlReturn_t (*pNvmlShutdown)(void) = NULL;
static nvmlReturn_t (*pNvmlDeviceGetHandle)(unsigned int, nvmlDevice_t*) = NULL;
static nvmlReturn_t (*pNvmlEncoderUtil)(nvmlDevice_t, unsigned int*, unsigned int*) = NULL;
static nvmlReturn_t (*pNvmlDecoderUtil)(nvmlDevice_t, unsigned int*, unsigned int*) = NULL;
static nvmlReturn_t (*pNvmlMemInfo)(nvmlDevice_t, nvmlMemory_t*) = NULL;

// Load libnvidia-ml.so at runtime and resolve function pointers.
// Returns 0 on success, negative on failure.
static int nvml_load() {
	nvml_lib = dlopen("libnvidia-ml.so.1", RTLD_LAZY);
	if (!nvml_lib) nvml_lib = dlopen("libnvidia-ml.so", RTLD_LAZY);
	if (!nvml_lib) return -1;

	pNvmlInit = dlsym(nvml_lib, "nvmlInit_v2");
	if (!pNvmlInit) pNvmlInit = dlsym(nvml_lib, "nvmlInit");
	pNvmlShutdown = dlsym(nvml_lib, "nvmlShutdown");

	pNvmlDeviceGetHandle = dlsym(nvml_lib, "nvmlDeviceGetHandleByIndex_v2");
	if (!pNvmlDeviceGetHandle) pNvmlDeviceGetHandle = dlsym(nvml_lib, "nvmlDeviceGetHandleByIndex");

	pNvmlEncoderUtil = dlsym(nvml_lib, "nvmlDeviceGetEncoderUtilization");
	pNvmlDecoderUtil = dlsym(nvml_lib, "nvmlDeviceGetDecoderUtilization");
	pNvmlMemInfo = dlsym(nvml_lib, "nvmlDeviceGetMemoryInfo");

	if (!pNvmlInit || !pNvmlDeviceGetHandle) return -2;
	return pNvmlInit();
}

static int nvml_get_device(unsigned int idx, nvmlDevice_t* dev) {
	if (!pNvmlDeviceGetHandle) return -1;
	return pNvmlDeviceGetHandle(idx, dev);
}

static int nvml_encoder_util(nvmlDevice_t dev, unsigned int* util, unsigned int* period) {
	if (!pNvmlEncoderUtil) return -1;
	return pNvmlEncoderUtil(dev, util, period);
}

static int nvml_decoder_util(nvmlDevice_t dev, unsigned int* util, unsigned int* period) {
	if (!pNvmlDecoderUtil) return -1;
	return pNvmlDecoderUtil(dev, util, period);
}

static int nvml_mem_info(nvmlDevice_t dev, nvmlMemory_t* mem) {
	if (!pNvmlMemInfo) return -1;
	return pNvmlMemInfo(dev, mem);
}
*/
import "C"

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
)

var (
	nvmlInitOnce sync.Once
	nvmlInitErr  error
)

func initNVML() error {
	nvmlInitOnce.Do(func() {
		ret := C.nvml_load()
		if ret != 0 {
			nvmlInitErr = fmt.Errorf("NVML init failed (code %d): libnvidia-ml.so not found or init error", int(ret))
		} else {
			glog.Info("NVML initialized successfully")
		}
	})
	return nvmlInitErr
}

// NvidiaMonitor tracks NVIDIA GPU encoder/decoder utilization and VRAM via NVML.
// NVML is loaded at runtime via dlopen — no link-time dependency on the NVIDIA driver.
type NvidiaMonitor struct {
	mu        sync.RWMutex
	device    C.nvmlDevice_t
	deviceIdx int
	encUtil   float64
	decUtil   float64
	memFree   uint64
	done      chan struct{}
}

// NewNvidiaMonitor creates an NVIDIA GPU monitor for the given device index string (e.g. "0", "1").
func NewNvidiaMonitor(device string) (HWMonitor, error) {
	if err := initNVML(); err != nil {
		return nil, fmt.Errorf("NVML unavailable: %w", err)
	}

	idx, err := strconv.Atoi(device)
	if err != nil {
		return nil, fmt.Errorf("invalid NVIDIA device index %q: %w", device, err)
	}

	var dev C.nvmlDevice_t
	ret := C.nvml_get_device(C.uint(idx), &dev)
	if ret != 0 {
		return nil, fmt.Errorf("NVML: device %d not found (code %d)", idx, int(ret))
	}

	return &NvidiaMonitor{
		device:    dev,
		deviceIdx: idx,
		memFree:   math.MaxUint64,
		done:      make(chan struct{}),
	}, nil
}

func (m *NvidiaMonitor) EncoderUtil() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.encUtil
}

func (m *NvidiaMonitor) DecoderUtil() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.decUtil
}

func (m *NvidiaMonitor) MemoryAvailable() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.memFree
}

// ActiveSessions returns 0 — orchs patch drivers for unlimited sessions, so tracking isn't useful.
func (m *NvidiaMonitor) ActiveSessions() int { return 0 }

// MaxHWSessions returns MaxInt — orchs patch NVIDIA drivers for unlimited encode sessions.
func (m *NvidiaMonitor) MaxHWSessions() int { return math.MaxInt }

func (m *NvidiaMonitor) Start() error {
	go m.pollLoop()
	return nil
}

func (m *NvidiaMonitor) Stop() {
	close(m.done)
}

func (m *NvidiaMonitor) pollLoop() {
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

func (m *NvidiaMonitor) sample() {
	var encUtil, encPeriod C.uint
	var decUtil, decPeriod C.uint
	var memInfo C.nvmlMemory_t

	var enc, dec float64
	var memFree uint64 = math.MaxUint64

	if C.nvml_encoder_util(m.device, &encUtil, &encPeriod) == 0 {
		enc = float64(encUtil) / 100.0
	}
	if C.nvml_decoder_util(m.device, &decUtil, &decPeriod) == 0 {
		dec = float64(decUtil) / 100.0
	}
	if C.nvml_mem_info(m.device, &memInfo) == 0 {
		memFree = uint64(memInfo.free)
	}

	m.mu.Lock()
	m.encUtil = enc
	m.decUtil = dec
	m.memFree = memFree
	m.mu.Unlock()
}

func init() {
	RegisterHWMonitorFactory(ffmpeg.Nvidia, NewNvidiaMonitor)
}
