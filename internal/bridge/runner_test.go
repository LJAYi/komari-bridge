package bridge

import (
	"testing"

	"github.com/LJAYi/komari-bridge/internal/model"
)

func TestMergeSnapshotsUsesGuestMetricsAndKeepsTopology(t *testing.T) {
	t.Parallel()
	identity := model.Identity{SourceType: "proxmox", SourceID: "site-a", ExternalID: "qemu:105"}
	pve := model.Snapshot{
		Identity: identity, Name: "gpu-a", Group: "Lab A", ResourceType: "qemu",
		ParentExternalID: "node:pve-a", Tags: map[string]string{"node": "pve-a"},
		BasicInfo: model.BasicInfo{OS: "Ubuntu", MemoryTotal: 108},
		Report:    model.Report{RAM: model.Usage{Total: 108, Used: 106}},
		Priority:  10,
	}
	guest := model.Snapshot{
		Identity: identity, Tags: map[string]string{"metrics_source": "ssh"},
		BasicInfo: model.BasicInfo{OS: "Ubuntu 24.04.4 LTS", CPUName: "Example CPU", MemoryTotal: 114},
		Report:    model.Report{RAM: model.Usage{Total: 114, Used: 7}, GPU: &model.GPUReport{Count: 4}},
		Priority:  100,
	}

	got := mergeSnapshots(pve, guest)
	if got.Name != "gpu-a" || got.ResourceType != "qemu" || got.ParentExternalID != "node:pve-a" {
		t.Fatalf("topology metadata was not preserved: %#v", got)
	}
	if got.BasicInfo.CPUName != "Example CPU" || got.Report.RAM.Used != 7 || got.Report.GPU == nil || got.Report.GPU.Count != 4 {
		t.Fatalf("guest metrics were not selected: %#v", got)
	}
	if got.Tags["node"] != "pve-a" || got.Tags["metrics_source"] != "ssh" {
		t.Fatalf("tags were not merged: %#v", got.Tags)
	}
}
