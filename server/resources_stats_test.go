package server

import (
	"testing"

	"github.com/pterodactyl/wings/environment"
)

func TestResetClearsOptionalResourceCounters(t *testing.T) {
	value := uint64(42)
	usage := ResourceUsage{Stats: environment.Stats{
		DiskReadBytes: &value, DiskWriteBytes: &value, BootID: "old-container",
	}}
	usage.Reset()
	if usage.DiskReadBytes != nil || usage.DiskWriteBytes != nil || usage.BootID != "" {
		t.Fatal("stopped server retained counters from the previous container")
	}
}
