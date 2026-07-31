package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
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
}

func TestParseGuestMemoryRejectsMissingAvailable(t *testing.T) {
	t.Parallel()
	if _, err := parseGuestMemory("MemTotal: 1024 kB\n"); err == nil {
		t.Fatal("parseGuestMemory accepted missing MemAvailable")
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
