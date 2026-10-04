//go:build linux

package core

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCgroupHost lays out a cgroup v2 tree, /proc/self/cgroup and /proc/stat
// under a temp dir for one process in system.slice/livepeer.service.
type fakeCgroupHost struct {
	t                          *testing.T
	root, procCgroup, procStat string
	own, slice                 string
}

func newFakeCgroupHost(t *testing.T) *fakeCgroupHost {
	dir := t.TempDir()
	h := &fakeCgroupHost{
		t:          t,
		root:       filepath.Join(dir, "cgroup"),
		procCgroup: filepath.Join(dir, "self-cgroup"),
		procStat:   filepath.Join(dir, "stat"),
	}
	h.slice = filepath.Join(h.root, "system.slice")
	h.own = filepath.Join(h.slice, "livepeer.service")
	require.NoError(t, os.MkdirAll(h.own, 0o755))
	h.write(h.procCgroup, "0::/system.slice/livepeer.service\n")
	h.write(filepath.Join(h.own, "cpu.max"), "max 100000\n")
	h.write(filepath.Join(h.slice, "cpu.max"), "max 100000\n")
	h.usage(h.own, 0)
	h.usage(h.slice, 0)
	h.hostStat(0, 0)
	return h
}

func (h *fakeCgroupHost) write(path, content string) {
	require.NoError(h.t, os.WriteFile(path, []byte(content), 0o644))
}

func (h *fakeCgroupHost) usage(dir string, usec uint64) {
	h.write(filepath.Join(dir, "cpu.stat"), "usage_usec "+strconv.FormatUint(usec, 10)+"\nuser_usec 0\nsystem_usec 0\n")
}

// hostStat writes /proc/stat with the given busy (user) and idle jiffies.
func (h *fakeCgroupHost) hostStat(busy, idle uint64) {
	h.write(h.procStat, "cpu  "+strconv.FormatUint(busy, 10)+" 0 0 "+strconv.FormatUint(idle, 10)+" 0 0 0 0 0 0\ncpu0 0 0 0 0 0 0 0 0 0 0\n")
}

func (h *fakeCgroupHost) sampler() *cpuSampler {
	return newCPUSampler(h.root, h.procCgroup, h.procStat)
}

func TestCPUSampler_QuotaMeasuresOwnUsageNotHost(t *testing.T) {
	h := newFakeCgroupHost(t)
	h.write(filepath.Join(h.own, "cpu.max"), "200000 100000\n") // 2 CPUs
	s := h.sampler()
	t0 := time.Unix(1000, 0)
	_, ok := s.next(t0)
	assert.False(t, ok, "the first reading has nothing to compare with")

	// The host is fully busy with other work; this process used 0.4 of its 2 CPUs.
	h.hostStat(1000, 0)
	h.usage(h.own, 400_000)
	util, ok := s.next(t0.Add(time.Second))
	require.True(t, ok)
	assert.InDelta(t, 0.2, util, 1e-9)
}

func TestCPUSampler_TightestQuotaOnThePathWins(t *testing.T) {
	h := newFakeCgroupHost(t)
	h.write(filepath.Join(h.own, "cpu.max"), "400000 100000\n")   // 4 CPUs for this service
	h.write(filepath.Join(h.slice, "cpu.max"), "100000 100000\n") // 1 CPU for the whole slice
	s := h.sampler()
	t0 := time.Unix(1000, 0)
	s.next(t0)

	// This service used 0.3 CPU; its siblings brought the slice to 0.9 of 1 CPU.
	h.usage(h.own, 300_000)
	h.usage(h.slice, 900_000)
	util, ok := s.next(t0.Add(time.Second))
	require.True(t, ok)
	assert.InDelta(t, 0.9, util, 1e-9)
}

func TestCPUSampler_NoQuotaMeasuresHost(t *testing.T) {
	h := newFakeCgroupHost(t)
	s := h.sampler()
	t0 := time.Unix(1000, 0)
	s.next(t0)

	h.hostStat(75, 25)
	util, ok := s.next(t0.Add(time.Second))
	require.True(t, ok)
	assert.InDelta(t, 0.75, util, 1e-9)
}

func TestCPUSampler_QuotaChangeAppliesAtOnce(t *testing.T) {
	h := newFakeCgroupHost(t)
	s := h.sampler()
	t0 := time.Unix(1000, 0)
	s.next(t0)
	h.hostStat(10, 90)
	s.next(t0.Add(time.Second))

	// A container limit is set at runtime: the next reading starts the cgroup
	// series, the one after measures against the quota.
	h.write(filepath.Join(h.own, "cpu.max"), "100000 100000\n")
	h.usage(h.own, 1_000_000)
	_, ok := s.next(t0.Add(2 * time.Second))
	assert.False(t, ok)
	h.usage(h.own, 1_500_000)
	util, ok := s.next(t0.Add(3 * time.Second))
	require.True(t, ok)
	assert.InDelta(t, 0.5, util, 1e-9)
}

func TestCPUSampler_ContainerNamespaceRoot(t *testing.T) {
	// Inside a container's cgroup namespace the process sits at "/" and the
	// container's own limit is on the mount root.
	h := newFakeCgroupHost(t)
	h.write(h.procCgroup, "0::/\n")
	h.write(filepath.Join(h.root, "cpu.max"), "200000 100000\n")
	h.usage(h.root, 0)
	s := h.sampler()
	t0 := time.Unix(1000, 0)
	s.next(t0)

	h.hostStat(1000, 0)
	h.usage(h.root, 1_000_000)
	util, ok := s.next(t0.Add(time.Second))
	require.True(t, ok)
	assert.InDelta(t, 0.5, util, 1e-9)
}

func TestReadCgroupCPUMax(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]float64{"max 100000\n": -1, "150000 100000\n": 1.5, "0 100000": -1, "garbage": -1} {
		path := filepath.Join(dir, "cpu.max")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		cpus, ok := readCgroupCPUMax(path)
		if want < 0 {
			assert.False(t, ok, content)
			continue
		}
		require.True(t, ok, content)
		assert.InDelta(t, want, cpus, 1e-9, content)
	}
}
