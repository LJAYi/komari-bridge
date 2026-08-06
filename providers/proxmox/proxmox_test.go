package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
)

func TestCollect(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=user@pve!bridge=secret" {
			t.Errorf("unexpected authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			w.Write([]byte(`{"data":[
          {"id":"node/pve-a","type":"node","node":"pve-a","status":"online","cpu":0.25,"maxcpu":8,"mem":100,"maxmem":1000,"disk":20,"maxdisk":200,"uptime":99},
          {"id":"qemu/105","type":"qemu","node":"pve-a","name":"ubuntu","vmid":105,"status":"running","cpu":0.5,"maxcpu":128,"mem":500,"maxmem":1000,"disk":25,"maxdisk":200,"netin":10,"netout":20},
          {"id":"qemu/106","type":"qemu","node":"pve-a","name":"stopped-test","vmid":106,"status":"stopped","maxcpu":2,"maxmem":1000},
          {"id":"qemu/9000","type":"qemu","node":"pve-a","name":"template","vmid":9000,"template":1}
        ]}`))
		case "/api2/json/nodes/pve-a/status":
			// PVE 9 returns cpuinfo.mhz as a JSON string. It is intentionally
			// present here to prevent an unused field from breaking enrichment.
			w.Write([]byte(`{"data":{"cpuinfo":{"model":"Example 32-Core Processor","cpus":128,"cores":64,"sockets":2,"mhz":"3791.478"},"pveversion":"pve-manager/9.2.2","kversion":"Linux 7.0.2-6-pve"}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/get-osinfo":
			w.Write([]byte(`{"data":{"result":{"name":"Ubuntu","pretty-name":"Ubuntu 24.04.4 LTS","machine":"x86_64","kernel-release":"6.8.0"}}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/get-fsinfo":
			w.Write([]byte(`{"data":{"result":[
              {"name":"sda1","mountpoint":"/","type":"ext4","total-bytes":1000,"used-bytes":400,"disk":[{"dev":"/dev/sda1","serial":"drive-scsi0"}]},
              {"name":"sdb1","mountpoint":"/data","type":"xfs","total-bytes":4000,"used-bytes":1000}
            ]}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/exec":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			commands := r.Form["command"]
			if r.Method != http.MethodPost || len(commands) != 2 || commands[0] != "/bin/cat" || commands[1] != "/proc/meminfo" {
				t.Errorf("unexpected QGA exec request: method=%s command=%q", r.Method, commands)
			}
			w.Write([]byte(`{"data":{"pid":42}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/exec-status":
			if r.URL.Query().Get("pid") != "42" {
				t.Errorf("unexpected QGA pid: %q", r.URL.Query().Get("pid"))
			}
			w.Write([]byte(`{"data":{"exited":1,"exitcode":0,"out-data":"MemTotal: 1000 kB\nMemAvailable: 750 kB\nSwapTotal: 100 kB\nSwapFree: 80 kB\n"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(config.ProxmoxConfig{
		ID: "site-a", Endpoint: server.URL, TokenID: "user@pve!bridge", TokenSecret: "secret",
		Group: "Lab A", InsecureSkipVerify: true, SkipStopped: true, Names: map[string]string{"qemu:105": "gpu-a"},
		Resources: map[string]config.ResourceOverride{"qemu:105": {GuestMemory: "qga"}},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snapshots))
	}
	node := snapshots[0]
	if node.BasicInfo.CPUName != "Example 32-Core Processor × 2" || node.BasicInfo.CPUCores != 128 || node.BasicInfo.CPUPhysicalCores != 64 || node.BasicInfo.Arch != "x86_64" {
		t.Fatalf("unexpected node CPU info: %#v", node.BasicInfo)
	}
	vm := snapshots[1]
	if vm.Name != "gpu-a" || vm.ParentExternalID != "node:pve-a" || vm.Report.CPU.Usage != 50 || vm.BasicInfo.OS != "Ubuntu 24.04.4 LTS" {
		t.Fatalf("unexpected VM snapshot: %#v", vm)
	}
	if vm.Report.RAM.Total != 1000*1024 || vm.Report.RAM.Used != 250*1024 || vm.Report.Swap.Used != 20*1024 || vm.Tags["memory_source"] != "qga_proc_meminfo" {
		t.Fatalf("unexpected QGA memory snapshot: %#v", vm)
	}
	if vm.BasicInfo.DiskTotal != 5000 || vm.Report.Disk != (model.Usage{Total: 5000, Used: 1400}) || vm.Tags["disk_source"] != "qga_get_fsinfo" {
		t.Fatalf("unexpected QGA guest disk usage: %#v", vm)
	}
	if len(vm.Report.Disks) != 2 || vm.Report.Disks[0].Mountpoint != "/" || vm.Report.Disks[0].Device != "/dev/sda1" || vm.Report.Disks[1].Mountpoint != "/data" {
		t.Fatalf("unexpected QGA mountpoint report: %#v", vm.Report.Disks)
	}
}

func TestCollectExcludesResource(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api2/json/cluster/resources" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
          {"id":"node/pve-a","type":"node","node":"pve-a","status":"online"},
          {"id":"qemu/105","type":"qemu","node":"pve-a","name":"ubuntu","vmid":105,"status":"running"}
        ]}`))
	}))
	defer server.Close()

	p, err := New(config.ProxmoxConfig{
		ID: "site-a", Endpoint: server.URL, TokenID: "token", TokenSecret: "secret",
		InsecureSkipVerify: true, ExcludeResources: []string{"node:pve-a"},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Identity.ExternalID != "qemu:105" {
		t.Fatalf("unexpected snapshots: %#v", snapshots)
	}
}

func TestSummarizeGuestFilesystems(t *testing.T) {
	t.Parallel()
	disk, err := summarizeGuestFilesystems([]guestFilesystem{
		{Name: "sda1", Mountpoint: "/", Type: "ext4", TotalBytes: 1000, UsedBytes: 400},
		{Name: "sda1", Mountpoint: "/bind", Type: "ext4", TotalBytes: 1000, UsedBytes: 400},
		{Name: "loop0", Mountpoint: "/rom", Type: "squashfs", TotalBytes: 100, UsedBytes: 100},
		{Name: "overlay", Mountpoint: "/overlay", Type: "f2fs", TotalBytes: 2000, UsedBytes: 300},
	})
	if err != nil {
		t.Fatal(err)
	}
	if disk.Total != 3000 || disk.Used != 700 || len(disk.Mounts) != 2 || disk.Mounts[0].Mountpoint != "/" || disk.Mounts[1].Mountpoint != "/overlay" {
		t.Fatalf("unexpected filesystem summary: %#v", disk)
	}
}

func TestNetworkRates(t *testing.T) {
	t.Parallel()
	p := &Provider{network: make(map[string]networkSample)}
	started := time.Unix(100, 0)
	if up, down := p.networkRates("qemu:100", 1000, 2000, started); up != 0 || down != 0 {
		t.Fatalf("first sample rates = %d/%d, want 0/0", up, down)
	}
	if up, down := p.networkRates("qemu:100", 3000, 5000, started.Add(20*time.Second)); up != 100 || down != 150 {
		t.Fatalf("second sample rates = %d/%d, want 100/150", up, down)
	}
	if up, down := p.networkRates("qemu:100", 10, 20, started.Add(40*time.Second)); up != 0 || down != 0 {
		t.Fatalf("reset sample rates = %d/%d, want 0/0", up, down)
	}
}

func TestParseGuestMemoryRejectsMissingAvailable(t *testing.T) {
	t.Parallel()
	if _, err := parseGuestMemory("MemTotal: 1024 kB\n"); err == nil {
		t.Fatal("parseGuestMemory accepted missing MemAvailable")
	}
}

func TestParseGuestMemoryRejectsInconsistentTotals(t *testing.T) {
	t.Parallel()
	for _, contents := range []string{
		"MemTotal: 1024 kB\nMemAvailable: 2048 kB\n",
		"MemTotal: 1024 kB\nMemAvailable: 512 kB\nSwapTotal: 128 kB\n",
		"MemTotal: 1024 kB\nMemAvailable: 512 kB\nSwapTotal: 128 kB\nSwapFree: 256 kB\n",
	} {
		if _, err := parseGuestMemory(contents); err == nil {
			t.Fatalf("parseGuestMemory(%q) succeeded", contents)
		}
	}
}

func TestCPUDisplayName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		model   string
		sockets int
		want    string
	}{
		{name: "single socket", model: "Example CPU", sockets: 1, want: "Example CPU"},
		{name: "dual socket", model: "Example CPU", sockets: 2, want: "Example CPU × 2"},
		{name: "missing socket count", model: "Intel Xeon", sockets: 0, want: "Intel Xeon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cpuDisplayName(test.model, test.sockets); got != test.want {
				t.Fatalf("cpuDisplayName(%q, %d) = %q, want %q", test.model, test.sockets, got, test.want)
			}
		})
	}
}

func TestFirstNodeStatusFailureDoesNotPublishGenericCPU(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api2/json/cluster/resources" {
			w.Write([]byte(`{"data":[{"id":"node/pve-a","type":"node","node":"pve-a","status":"online","maxcpu":128,"maxmem":1000,"maxdisk":200}]}`))
			return
		}
		http.Error(w, "temporary status failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	p, err := New(config.ProxmoxConfig{ID: "site-a", Endpoint: server.URL, TokenID: "token", TokenSecret: "secret", InsecureSkipVerify: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Online {
		t.Fatalf("node without status enrichment was published: %#v", snapshots)
	}
}

func TestValidGuestOSRequiresCompleteIdentity(t *testing.T) {
	t.Parallel()
	var info guestOSResponse
	info.Data.Result.Name = "Ubuntu"
	if validGuestOS(info) {
		t.Fatal("partial QGA OS identity was accepted")
	}
	info.Data.Result.Machine = "x86_64"
	info.Data.Result.KernelRelease = "6.8.0"
	if !validGuestOS(info) {
		t.Fatal("complete QGA OS identity was rejected")
	}
}

func TestQGAMemoryFirstFailureMarksSnapshotOffline(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			w.Write([]byte(`{"data":[{"id":"qemu/105","type":"qemu","node":"pve-a","name":"ubuntu","vmid":105,"status":"running","mem":900,"maxmem":1000}]}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/get-osinfo":
			http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		case "/api2/json/nodes/pve-a/qemu/105/agent/exec":
			http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := newQGATestProvider(t, server.URL)
	snapshots, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.Online {
		t.Fatal("snapshot is online despite required QGA memory being unavailable")
	}
	if snapshot.Report.RAM.Total != 0 || snapshot.Report.RAM.Used != 0 || snapshot.BasicInfo.MemoryTotal != 0 {
		t.Fatalf("PVE memory leaked into QGA-required snapshot: %#v", snapshot)
	}
	if snapshot.Tags["memory_source"] != "qga_unavailable" {
		t.Fatalf("unexpected memory source: %q", snapshot.Tags["memory_source"])
	}
}

func TestQGAMemoryTransientFailureUsesLastKnownGood(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			w.Write([]byte(`{"data":[{"id":"qemu/105","type":"qemu","node":"pve-a","name":"ubuntu","vmid":105,"status":"running","mem":999,"maxmem":1000}]}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/get-osinfo":
			w.Write([]byte(`{"data":{"result":{"name":"Ubuntu"}}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/exec":
			if fail.Load() {
				http.Error(w, "temporary QGA failure", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"data":{"pid":42}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/exec-status":
			w.Write([]byte(`{"data":{"exited":1,"exitcode":0,"out-data":"MemTotal: 1000 kB\nMemAvailable: 750 kB\nSwapTotal: 100 kB\nSwapFree: 80 kB\n"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := newQGATestProvider(t, server.URL)
	first := collectSingleSnapshot(t, p)
	fail.Store(true)
	second := collectSingleSnapshot(t, p)
	if !second.Online {
		t.Fatal("snapshot became offline despite cached QGA memory")
	}
	if second.Report.RAM != first.Report.RAM || second.Report.Swap != first.Report.Swap || second.BasicInfo.MemoryTotal != first.BasicInfo.MemoryTotal {
		t.Fatalf("cached QGA memory changed: first=%#v second=%#v", first, second)
	}
	if second.Report.RAM.Used == 999 {
		t.Fatal("snapshot fell back to PVE memory")
	}
	if second.Tags["memory_source"] != "qga_proc_meminfo_cached" {
		t.Fatalf("unexpected memory source: %q", second.Tags["memory_source"])
	}
	p.cacheMu.Lock()
	cached := p.guestMemory["qemu:105"]
	cached.savedAt = time.Now().Add(-guestEnrichmentGracePeriod - time.Second)
	p.guestMemory["qemu:105"] = cached
	p.cacheMu.Unlock()
	third := collectSingleSnapshot(t, p)
	if third.Online || third.Report.RAM.Total != 0 || third.Report.RAM.Used != 0 {
		t.Fatalf("expired QGA memory kept reporting or fell back to PVE: %#v", third)
	}
}

func TestNodeAndGuestOSEnrichmentSurviveTransientFailure(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			w.Write([]byte(`{"data":[
				{"id":"node/pve-a","type":"node","node":"pve-a","status":"online","maxcpu":128},
				{"id":"qemu/105","type":"qemu","node":"pve-a","name":"ubuntu","vmid":105,"status":"running","maxcpu":8}
			]}`))
		case "/api2/json/nodes/pve-a/status":
			if fail.Load() {
				http.Error(w, "temporary status failure", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"data":{"cpuinfo":{"model":"AMD EPYC 9354","cpus":128,"cores":64,"sockets":2},"pveversion":"pve-manager/9.2.2","kversion":"Linux 6.8-pve"}}`))
		case "/api2/json/nodes/pve-a/qemu/105/agent/get-osinfo":
			if fail.Load() {
				http.Error(w, "temporary agent failure", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"data":{"result":{"name":"Ubuntu","pretty-name":"Ubuntu 24.04 LTS","machine":"x86_64","kernel-release":"6.8.0"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(config.ProxmoxConfig{
		ID: "site-a", Endpoint: server.URL, TokenID: "token", TokenSecret: "secret", InsecureSkipVerify: true,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	second, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("unexpected snapshot counts: %d, %d", len(first), len(second))
	}
	if second[0].BasicInfo != first[0].BasicInfo {
		t.Fatalf("node enrichment degraded: first=%#v second=%#v", first[0].BasicInfo, second[0].BasicInfo)
	}
	if second[1].BasicInfo.OS != first[1].BasicInfo.OS || second[1].BasicInfo.Arch != first[1].BasicInfo.Arch || second[1].BasicInfo.KernelVersion != first[1].BasicInfo.KernelVersion {
		t.Fatalf("guest OS enrichment degraded: first=%#v second=%#v", first[1].BasicInfo, second[1].BasicInfo)
	}
}

func newQGATestProvider(t *testing.T, endpoint string) *Provider {
	t.Helper()
	p, err := New(config.ProxmoxConfig{
		ID: "site-a", Endpoint: endpoint, TokenID: "token", TokenSecret: "secret", InsecureSkipVerify: true,
		Resources: map[string]config.ResourceOverride{"qemu:105": {GuestMemory: "qga"}},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func collectSingleSnapshot(t *testing.T, p *Provider) model.Snapshot {
	t.Helper()
	snapshots, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	return snapshots[0]
}
