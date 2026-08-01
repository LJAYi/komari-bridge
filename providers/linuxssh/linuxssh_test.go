package linuxssh

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
	"github.com/LJAYi/komari-bridge/internal/slurm"
)

func TestCapabilityConstructors(t *testing.T) {
	t.Parallel()
	cfg := config.LinuxSSHConfig{ID: "host-a", User: "monitor", Password: "test-only", InsecureIgnoreHostKey: true}
	agentless, err := NewAgentless(cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if agentless.SourceType() != "agentless_ssh" || agentless.slurmOnly {
		t.Fatalf("unexpected agentless mode: %#v", agentless)
	}
	scheduler, err := NewSlurm(cfg, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scheduler.SourceType() != "slurm" || !scheduler.slurmOnly || !scheduler.cfg.EnableSlurm {
		t.Fatalf("unexpected Slurm mode: %#v", scheduler)
	}
}

func TestSlurmCollectorOmitsHostAndGPUCommands(t *testing.T) {
	t.Parallel()
	for _, unwanted := range []string{"nvidia-smi", "/proc/meminfo", "df"} {
		if strings.Contains(slurmCollectorScript, unwanted) {
			t.Fatalf("Slurm-only collector contains %q", unwanted)
		}
	}
	if !strings.Contains(slurmCollectorScript, "sinfo") || !strings.Contains(slurmCollectorScript, "squeue") {
		t.Fatal("Slurm-only collector is missing scheduler commands")
	}
}

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

func TestValidateRemoteReportRejectsDegradedHostData(t *testing.T) {
	t.Parallel()
	valid := remoteReport{
		CPUName: "CPU", CPUCores: 8, Arch: "x86_64", OS: "Linux", Kernel: "6.8",
		CPU:       cpuCounters{Total: 100, Idle: 50},
		Memory:    map[string]uint64{"MemTotal": 1024, "MemAvailable": 512},
		DiskTotal: 4096, DiskUsed: 1024, Uptime: 100, GPUOK: true,
	}
	if err := validateRemoteReport(valid, true); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*remoteReport)
	}{
		{"missing memory", func(r *remoteReport) { r.Memory = nil }},
		{"available exceeds total", func(r *remoteReport) { r.Memory["MemAvailable"] = 2048 }},
		{"zero disk", func(r *remoteReport) { r.DiskTotal = 0 }},
		{"disk used exceeds total", func(r *remoteReport) { r.DiskUsed = r.DiskTotal + 1 }},
		{"missing CPU", func(r *remoteReport) { r.CPUName = "" }},
		{"GPU command failure", func(r *remoteReport) { r.GPUOK = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := valid
			copy.Memory = map[string]uint64{"MemTotal": 1024, "MemAvailable": 512}
			test.mutate(&copy)
			if err := validateRemoteReport(copy, true); err == nil {
				t.Fatal("degraded report was accepted")
			}
		})
	}
}

func TestValidateRemoteReportDistinguishesNoGPUFromFailure(t *testing.T) {
	t.Parallel()
	report := remoteReport{
		CPUName: "CPU", CPUCores: 8, Arch: "x86_64", OS: "Linux", Kernel: "6.8",
		CPU: cpuCounters{Total: 100}, Memory: map[string]uint64{"MemTotal": 1024, "MemAvailable": 512},
		DiskTotal: 4096, Uptime: 100, GPUOK: true,
	}
	if len(report.GPUs) != 0 {
		t.Fatal("test report unexpectedly has GPUs")
	}
	if err := validateRemoteReport(report, true); err != nil {
		t.Fatalf("successful zero-GPU result rejected: %v", err)
	}
	report.GPUOK = false
	if err := validateRemoteReport(report, true); err == nil {
		t.Fatal("failed GPU command was accepted")
	}
}

func TestMarkSlurmUnavailableReplacesStaleSnapshot(t *testing.T) {
	t.Parallel()
	store := slurm.NewStore()
	store.Set("cluster-a", slurm.Snapshot{SourceID: "cluster-a", Available: true, JobsRunning: 3})
	p := &Provider{cfg: config.LinuxSSHConfig{ID: "cluster-a", EnableSlurm: true}, slurmStore: store}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	p.markSlurmUnavailable(now, "SSH collection failed")
	snapshot, ok := store.Get("cluster-a")
	if !ok || snapshot.Available || snapshot.Error != "SSH collection failed" || !snapshot.CollectedAt.Equal(now) || snapshot.JobsRunning != 0 {
		t.Fatalf("stale Slurm snapshot was not replaced: %#v", snapshot)
	}
}
