package linuxssh

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/slurm"
)

func TestIntegrationLinuxNVIDIASlurm(t *testing.T) {
	address := os.Getenv("KOMARI_BRIDGE_TEST_SSH_ADDRESS")
	keyPath := os.Getenv("KOMARI_BRIDGE_TEST_SSH_KEY")
	hostKey := os.Getenv("KOMARI_BRIDGE_TEST_SSH_HOST_KEY")
	if address == "" || keyPath == "" || hostKey == "" {
		t.Skip("set KOMARI_BRIDGE_TEST_SSH_ADDRESS, _KEY, and _HOST_KEY to run")
	}

	store := slurm.NewStore()
	provider, err := New(config.LinuxSSHConfig{
		ID: "integration-gpu-host", Address: address, User: "monitor",
		PrivateKeyPath: keyPath, HostKey: hostKey,
		EnableNVIDIA: true, EnableSlurm: true,
	}, 15*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snapshots, err := provider.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.BasicInfo.OS == "" || snapshot.BasicInfo.CPUName == "" || snapshot.Report.RAM.Total == 0 {
		t.Fatalf("missing basic guest metrics: %#v", snapshot)
	}
	if snapshot.Report.GPU == nil || snapshot.Report.GPU.Count == 0 || len(snapshot.Report.GPU.DetailedInfo) != snapshot.Report.GPU.Count {
		t.Fatalf("unexpected GPU report: %#v", snapshot.Report.GPU)
	}
	slurmSnapshot, ok := store.Get("integration-gpu-host")
	if !ok || len(slurmSnapshot.Partitions) == 0 {
		t.Fatalf("unexpected Slurm snapshot: %#v", slurmSnapshot)
	}
}
