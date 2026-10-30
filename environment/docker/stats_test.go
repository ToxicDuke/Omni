package docker

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func TestCalculateDockerBlockIO(t *testing.T) {
	for _, test := range []struct {
		name        string
		entries     []container.BlkioStatEntry
		read, write *uint64
	}{
		{name: "unsupported"},
		{name: "ignore totals", entries: []container.BlkioStatEntry{{Op: "Total", Value: 90}}},
		{name: "explicit zero", entries: []container.BlkioStatEntry{{Op: "Read", Value: 0}}, read: ioValue(0)},
		{name: "multiple devices", entries: []container.BlkioStatEntry{{Op: "Read", Value: 10}, {Op: "read", Value: 20}, {Op: "Write", Value: 5}, {Op: "Total", Value: 35}}, read: ioValue(30), write: ioValue(5)},
	} {
		t.Run(test.name, func(t *testing.T) {
			read, write := calculateDockerBlockIO(container.BlkioStats{IoServiceBytesRecursive: test.entries})
			for i, pair := range [][2]*uint64{{read, test.read}, {write, test.write}} {
				if (pair[0] == nil) != (pair[1] == nil) || (pair[0] != nil && *pair[0] != *pair[1]) {
					t.Fatalf("counter %d: got %v, want %v", i, pair[0], pair[1])
				}
			}
		})
	}
}

func ioValue(value uint64) *uint64 { return &value }

func TestDockerBootIDIncludesContainerStart(t *testing.T) {
	started := time.Date(2026, time.October, 30, 12, 0, 0, 123, time.UTC)
	first := dockerBootID("same-container", started)
	if first == dockerBootID("same-container", started.Add(time.Minute)) {
		t.Fatal("restarting an existing container retained the previous session identity")
	}
	if first != dockerBootID("same-container", started.In(time.FixedZone("other", 10800))) {
		t.Fatal("the same container start changed identity with timezone formatting")
	}
}
