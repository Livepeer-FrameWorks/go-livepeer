//go:build linux

package core

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

type cpuSample struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (s cpuSample) total() uint64 {
	return s.user + s.nice + s.system + s.idle + s.iowait + s.irq + s.softirq + s.steal
}

func (s cpuSample) busy() uint64 {
	return s.total() - s.idle - s.iowait
}

// CPUMonitor tracks CPU utilization and available memory on Linux via /proc.
type CPUMonitor struct {
	mu       sync.RWMutex
	cpuUtil  float64
	memAvail uint64
	done     chan struct{}
}

// NewCPUMonitor creates a CPU monitor for software transcoding on Linux.
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
	prev := readCPUSample()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			curr := readCPUSample()
			totalDelta := curr.total() - prev.total()
			busyDelta := curr.busy() - prev.busy()
			var util float64
			if totalDelta > 0 {
				util = float64(busyDelta) / float64(totalDelta)
			}
			memAvail := readMemAvailable()

			m.mu.Lock()
			m.cpuUtil = util
			m.memAvail = memAvail
			m.mu.Unlock()

			prev = curr
		}
	}
}

func readCPUSample() cpuSample {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) >= 8 {
				s := cpuSample{
					user:    parseUint64(fields[1]),
					nice:    parseUint64(fields[2]),
					system:  parseUint64(fields[3]),
					idle:    parseUint64(fields[4]),
					iowait:  parseUint64(fields[5]),
					irq:     parseUint64(fields[6]),
					softirq: parseUint64(fields[7]),
				}
				if len(fields) > 8 {
					s.steal = parseUint64(fields[8])
				}
				return s
			}
		}
	}
	return cpuSample{}
}

// readMemAvailable reads MemAvailable from /proc/meminfo (kernel 3.14+).
func readMemAvailable() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return math.MaxUint64
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb := parseUint64(fields[1])
				return kb * 1024
			}
		}
	}
	return math.MaxUint64
}

func parseUint64(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func init() {
	RegisterHWMonitorFactory(ffmpeg.Software, NewCPUMonitor)
}
