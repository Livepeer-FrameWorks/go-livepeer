//go:build linux

package core

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
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

// CPUMonitor tracks CPU utilization and available memory on Linux. Under a
// cgroup v2 CPU quota (a container CPU limit or a systemd CPUQuota) it measures
// usage against the tightest quota on this process's cgroup path, because that
// quota and not the host is what this process can use. Without a quota it
// measures the whole host via /proc/stat.
type CPUMonitor struct {
	mu       sync.RWMutex
	cpuUtil  float64
	memAvail uint64
	done     chan struct{}
	sampler  *cpuSampler
}

// NewCPUMonitor creates a CPU monitor for software transcoding on Linux.
func NewCPUMonitor(_ string) (HWMonitor, error) {
	return &CPUMonitor{
		done:     make(chan struct{}),
		memAvail: math.MaxUint64,
		sampler:  newCPUSampler("/sys/fs/cgroup", "/proc/self/cgroup", "/proc/stat"),
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
	m.sampler.next(time.Now())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case now := <-ticker.C:
			util, ok := m.sampler.next(now)
			memAvail := readMemAvailable()

			m.mu.Lock()
			if ok {
				m.cpuUtil = util
			}
			m.memAvail = memAvail
			m.mu.Unlock()
		}
	}
}

// cpuSampler turns successive CPU counters into a utilization between 0 and 1.
type cpuSampler struct {
	cgroupRoot string
	procCgroup string
	procStat   string

	prevAt     time.Time
	prevHost   cpuSample
	hasHost    bool
	prevCgroup map[string]uint64
}

func newCPUSampler(cgroupRoot, procCgroup, procStat string) *cpuSampler {
	return &cpuSampler{cgroupRoot: cgroupRoot, procCgroup: procCgroup, procStat: procStat, prevCgroup: map[string]uint64{}}
}

// next records the counters at now and returns the utilization since the
// previous call. ok is false when there is no previous reading to compare.
// Quotas are re-read every time, so a changed container limit applies at once.
func (s *cpuSampler) next(now time.Time) (util float64, ok bool) {
	wall := now.Sub(s.prevAt)
	first := s.prevAt.IsZero()
	s.prevAt = now

	quotas := cgroupCPUQuotas(s.cgroupRoot, s.procCgroup)
	if len(quotas) > 0 {
		s.hasHost = false
		seen := make(map[string]uint64, len(quotas))
		for _, q := range quotas {
			usage, readOK := readCgroupCPUUsage(q.dir)
			if !readOK {
				continue
			}
			seen[q.dir] = usage
			prev, had := s.prevCgroup[q.dir]
			if first || !had || usage < prev || wall <= 0 {
				continue
			}
			level := float64(usage-prev) / (float64(wall.Microseconds()) * q.cpus)
			util = math.Max(util, level)
			ok = true
		}
		s.prevCgroup = seen
		return math.Min(util, 1), ok
	}

	s.prevCgroup = map[string]uint64{}
	curr := readCPUSample(s.procStat)
	prev, had := s.prevHost, s.hasHost
	s.prevHost, s.hasHost = curr, true
	if !had {
		return 0, false
	}
	totalDelta := curr.total() - prev.total()
	if curr.total() < prev.total() || totalDelta == 0 {
		return 0, false
	}
	busyDelta := curr.busy() - prev.busy()
	return math.Min(float64(busyDelta)/float64(totalDelta), 1), true
}

// cgroupCPUQuota is a cgroup v2 directory whose cpu.max caps CPU at cpus CPUs.
type cgroupCPUQuota struct {
	dir  string
	cpus float64
}

// cgroupCPUQuotas returns every cgroup from this process's own up to the root
// that sets a CPU quota. A quota on an ancestor is shared with that ancestor's
// other children, so its usage is measured at that ancestor.
func cgroupCPUQuotas(root, procCgroup string) []cgroupCPUQuota {
	data, err := os.ReadFile(procCgroup)
	if err != nil {
		return nil
	}
	rel, found := "", false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel, found = strings.TrimPrefix(line, "0::"), true
			break
		}
	}
	if !found {
		return nil
	}
	root = filepath.Clean(root)
	dir := filepath.Join(root, filepath.Clean("/"+rel))
	var quotas []cgroupCPUQuota
	for {
		if cpus, ok := readCgroupCPUMax(filepath.Join(dir, "cpu.max")); ok {
			quotas = append(quotas, cgroupCPUQuota{dir: dir, cpus: cpus})
		}
		if dir == root || !strings.HasPrefix(dir, root+"/") {
			break
		}
		dir = filepath.Dir(dir)
	}
	return quotas
}

// readCgroupCPUMax parses cpu.max ("<quota> <period>" or "max <period>").
func readCgroupCPUMax(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		return 0, false
	}
	quota, qErr := strconv.ParseUint(fields[0], 10, 64)
	period, pErr := strconv.ParseUint(fields[1], 10, 64)
	if qErr != nil || pErr != nil || quota == 0 || period == 0 {
		return 0, false
	}
	return float64(quota) / float64(period), true
}

// readCgroupCPUUsage reads usage_usec from a cgroup's cpu.stat.
func readCgroupCPUUsage(dir string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			v, err := strconv.ParseUint(fields[1], 10, 64)
			return v, err == nil
		}
	}
	return 0, false
}

func readCPUSample(path string) cpuSample {
	f, err := os.Open(path)
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
