package linuxssh

import (
	"math"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/model"
)

func TestCounterPercent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                             string
		total, idle, prevTotal, prevIdle uint64
		want                             float64
	}{
		{name: "first sample", total: 100, idle: 80, want: 0},
		{name: "half busy", total: 200, idle: 130, prevTotal: 100, prevIdle: 80, want: 50},
		{name: "counter reset", total: 50, idle: 40, prevTotal: 100, prevIdle: 80, want: 0},
		{name: "invalid idle delta", total: 200, idle: 190, prevTotal: 100, prevIdle: 50, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := counterPercent(test.total, test.idle, test.prevTotal, test.prevIdle)
			if math.Abs(got-test.want) > 0.001 {
				t.Fatalf("counterPercent() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRates(t *testing.T) {
	t.Parallel()
	up, down := rates(
		networkCounters{Up: 3_000, Down: 5_000},
		networkCounters{Up: 1_000, Down: 1_000},
		2*time.Second,
	)
	if up != 1_000 || down != 2_000 {
		t.Fatalf("rates() = (%d, %d), want (1000, 2000)", up, down)
	}
}

func TestClampUsedUsesAvailableMemory(t *testing.T) {
	t.Parallel()
	if got := clampUsed(1000, 700); got != 300 {
		t.Fatalf("clampUsed() = %d, want 300", got)
	}
	if got := clampUsed(1000, 1100); got != 0 {
		t.Fatalf("invalid available memory produced %d, want 0", got)
	}
}

func TestFormatGPUName(t *testing.T) {
	t.Parallel()
	got := formatGPUName([]model.GPUDevice{
		{Name: "Example GPU"},
		{Name: "Example GPU"},
		{Name: " Example GPU "},
		{Name: "Example GPU"},
	})
	if got != "Example GPU × 4" {
		t.Fatalf("formatGPUName() = %q", got)
	}
}
