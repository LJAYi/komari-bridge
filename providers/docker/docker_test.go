package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
)

func TestCollectClassifiesDockerComposeAndSwarm(t *testing.T) {
	t.Parallel()
	var generation atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/containers/json":
			generation.Add(1)
			fmt.Fprint(w, `[
              {"Id":"aaaaaaaaaaaa1111","Names":["/redis"],"Image":"redis:7","State":"running","Labels":{}},
              {"Id":"bbbbbbbbbbbb2222","Names":["/demo-web-1"],"Image":"nginx:latest","State":"running","Labels":{"com.docker.compose.project":"demo","com.docker.compose.service":"web","com.docker.compose.container-number":"1"}},
              {"Id":"cccccccccccc3333","Names":["/api.2.task"],"Image":"api:v1","State":"running","Labels":{"com.docker.swarm.service.id":"service-id","com.docker.swarm.service.name":"api","com.docker.swarm.task.name":"api.2.task-id"}}
            ]`)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			g := generation.Load()
			fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":200},"system_cpu_usage":1000,"online_cpus":4},"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":0,"online_cpus":4},"memory_stats":{"usage":500,"limit":1000,"stats":{"inactive_file":100}},"networks":{"eth0":{"rx_bytes":%d,"tx_bytes":%d}},"pids_stats":{"current":3}}`, g*1000, g*2000)
		case strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"HostConfig":{"NanoCpus":2000000000},"State":{"StartedAt":"2026-01-01T00:00:00Z","Health":{"Status":"healthy"}},"RestartCount":2}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(config.DockerConfig{
		ID: "engine-a", Endpoint: server.URL, InsecureAllowHTTP: true, IncludeAll: true, Group: "Lab A",
		AttachTo: config.ResourceIdentity{SourceType: "proxmox", SourceID: "site-a", ExternalID: "qemu:100"},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(first))
	}
	byID := make(map[string]int)
	for index, snapshot := range first {
		byID[snapshot.Identity.ExternalID] = index
		if snapshot.ParentExternalID != "qemu:100" || !snapshot.Online {
			t.Fatalf("unexpected topology/status: %#v", snapshot)
		}
		if snapshot.Report.RAM.Total != 1000 || snapshot.Report.RAM.Used != 400 || snapshot.Report.CPU.Usage != 20 || snapshot.BasicInfo.CPUCores != 2 {
			t.Fatalf("unexpected metrics: %#v", snapshot.Report)
		}
		if snapshot.Tags["docker_health"] != "healthy" || snapshot.Tags["docker_restart_count"] != "2" {
			t.Fatalf("missing inspect metadata: %#v", snapshot.Tags)
		}
	}
	for _, id := range []string{"container:redis", "compose:demo:web:1", "swarm:service-id:2"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing identity %q in %#v", id, byID)
		}
	}
	if got := first[byID["compose:demo:web:1"]]; got.ResourceType != "docker_compose_container" || got.Tags["docker_compose_project"] != "demo" {
		t.Fatalf("unexpected Compose resource: %#v", got)
	}
	if got := first[byID["swarm:service-id:2"]]; got.ResourceType != "docker_swarm_task" || got.Tags["docker_swarm_slot"] != "2" {
		t.Fatalf("unexpected Swarm resource: %#v", got)
	}

	time.Sleep(time.Millisecond)
	second, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range second {
		if snapshot.Report.Network.Up <= 0 || snapshot.Report.Network.Down <= 0 {
			t.Fatalf("network rates did not advance: %#v", snapshot.Report.Network)
		}
	}
}

func TestSelectedContainersRequiresOptInLabel(t *testing.T) {
	t.Parallel()
	containers := []containerSummary{
		{ID: "a", Labels: map[string]string{}},
		{ID: "b", Labels: map[string]string{"komari.bridge.monitor": "true"}},
	}
	selected := selectedContainers(containers, false)
	if len(selected) != 1 || selected[0].ID != "b" {
		t.Fatalf("selected containers = %#v", selected)
	}
}

func TestRejectsUnprotectedDockerHTTP(t *testing.T) {
	t.Parallel()
	if _, err := New(config.DockerConfig{ID: "unsafe", Endpoint: "http://docker.example:2375"}, time.Second); err == nil || !strings.Contains(err.Error(), "insecure_allow_http") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestSwarmSlot(t *testing.T) {
	t.Parallel()
	if got := swarmSlot("api.production.12.task-id"); got != "12" {
		t.Fatalf("swarmSlot() = %q, want 12", got)
	}
}

func TestCPUSetCount(t *testing.T) {
	t.Parallel()
	if got := cpuSetCount("0-2,4,6-7"); got != 6 {
		t.Fatalf("cpuSetCount() = %d, want 6", got)
	}
}
