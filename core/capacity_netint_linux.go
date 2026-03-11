//go:build linux

package core

import (
	"math"

	"github.com/livepeer/lpms/ffmpeg"
)

// NetintMonitor is a placeholder until Netint XCODER SDK integration is implemented.
// It returns permissive defaults so that the CapacityManager's EMA of realtime ratio
// (the self-calibrating signal updated after each transcode) still provides dynamic
// capacity tracking even without hardware counters from the Netint device.
type NetintMonitor struct{}

// NewNetintMonitor creates a Netint monitor. Returns stub-like behavior since
// the Netint XCODER SDK is not linked.
func NewNetintMonitor(_ string) (HWMonitor, error) {
	return &NetintMonitor{}, nil
}

func (m *NetintMonitor) EncoderUtil() float64    { return 0.0 }
func (m *NetintMonitor) DecoderUtil() float64    { return 0.0 }
func (m *NetintMonitor) MemoryAvailable() uint64 { return math.MaxUint64 }
func (m *NetintMonitor) ActiveSessions() int     { return 0 }
func (m *NetintMonitor) MaxHWSessions() int      { return math.MaxInt }
func (m *NetintMonitor) Start() error            { return nil }
func (m *NetintMonitor) Stop()                   {}

func init() {
	RegisterHWMonitorFactory(ffmpeg.Netint, NewNetintMonitor)
}
