package sandlock

import (
	"context"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
)

// ResourceSource reports the Sandlock backend to the existing Recorder.
// It does not sample cgroup counters. MemoryLimitBytes is the configured
// Sandlock MaxMemory when one was requested; usage stays zero because
// Sandlock's seccomp accounting is not a cgroup gauge.
type ResourceSource struct {
	taskID     string
	name       string
	limitBytes uint64
}

// NewResourceSource returns a Recorder source for one Sandlock occurrence.
func NewResourceSource(taskID, name string, memoryMB int) *ResourceSource {
	var limit uint64
	if memoryMB > 0 {
		limit = uint64(memoryMB) << 20
	}
	return &ResourceSource{taskID: taskID, name: name, limitBytes: limit}
}

// Sample returns one neutral reading identifying the sandlock backend.
func (s *ResourceSource) Sample(context.Context) ([]core.ResourceReading, error) {
	if s == nil {
		return nil, nil
	}
	return []core.ResourceReading{{
		TaskID:           s.taskID,
		Component:        "sandbox",
		RuntimeID:        s.name,
		RuntimeName:      "sandlock",
		ObservedAt:       time.Now(),
		MemoryLimitBytes: s.limitBytes,
	}}, nil
}

// Close has no host handle to release.
func (s *ResourceSource) Close() error { return nil }

var _ monitor.ResourceSource = (*ResourceSource)(nil)
