package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/buildinfo"
	"github.com/LJAYi/komari-bridge/internal/komari"
	"github.com/LJAYi/komari-bridge/internal/model"
	"github.com/LJAYi/komari-bridge/internal/provider"
	"github.com/LJAYi/komari-bridge/internal/store"
)

type scriptedProvider struct {
	id         string
	sourceType string
	targets    []model.Identity
	collect    func() ([]model.Snapshot, error)
}

func (p *scriptedProvider) ID() string         { return p.id }
func (p *scriptedProvider) SourceType() string { return p.sourceType }
func (p *scriptedProvider) MetricTargets() []model.Identity {
	return p.targets
}
func (p *scriptedProvider) Collect(context.Context) ([]model.Snapshot, error) {
	return p.collect()
}

type rpcRecorder struct {
	mu      sync.Mutex
	methods []string
	infos   []model.BasicInfo
}

func (r *rpcRecorder) handler(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/api/clients/register":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success", "data": map[string]string{"uuid": "uuid-1", "token": "token-1"},
		})
	case "/api/clients/v2/rpc":
		body, _ := io.ReadAll(request.Body)
		var payload struct {
			Method string `json:"method"`
			Params struct {
				Info model.BasicInfo `json:"info"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.methods = append(r.methods, payload.Method)
		if payload.Method == "agent.basicInfo" {
			r.infos = append(r.infos, payload.Params.Info)
		}
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": map[string]any{}})
	default:
		http.NotFound(w, request)
	}
}

func (r *rpcRecorder) snapshot() ([]string, []model.BasicInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...), append([]model.BasicInfo(nil), r.infos...)
}

func newTestRunner(t *testing.T, providers []provider.Provider, recorder *rpcRecorder) (*Runner, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	db, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	client, err := komari.New(server.URL, "discovery", time.Second)
	if err != nil {
		db.Close()
		server.Close()
		t.Fatal(err)
	}
	runner := NewRunner(db, client, providers, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return runner, func() { _ = db.Close(); server.Close() }
}

func authoritativeFixtures(identity model.Identity) (model.Snapshot, model.Snapshot) {
	pve := model.Snapshot{
		Identity: identity, Name: "tju-ev17", Online: true, Priority: 10,
		BasicInfo: model.BasicInfo{MemoryTotal: 100 << 30, DiskTotal: 480 << 30},
		Report: model.Report{
			RAM:  model.Usage{Total: 100 << 30, Used: 100 << 30},
			Disk: model.Usage{Total: 480 << 30, Used: 470 << 30},
		},
	}
	windows := model.Snapshot{
		Identity: identity, Name: "tju-ev17", Online: true, Priority: 100,
		BasicInfo: model.BasicInfo{OS: "Windows", MemoryTotal: 100 << 30, DiskTotal: 6 << 40},
		Report: model.Report{
			RAM:  model.Usage{Total: 100 << 30, Used: 40 << 30},
			Disk: model.Usage{Total: 6 << 40, Used: 3 << 40},
		},
	}
	return pve, windows
}

func TestAuthoritySuppressesFirstCycleFallbackAndReusesRecentSnapshot(t *testing.T) {
	identity := model.Identity{SourceType: "proxmox", SourceID: "tju-1", ExternalID: "qemu:107"}
	pve, windows := authoritativeFixtures(identity)
	windowsErr := errors.New("ssh transport closed")
	currentWindows := []model.Snapshot(nil)
	currentErr := windowsErr
	pveProvider := &scriptedProvider{id: "tju-1", sourceType: "proxmox", collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{pve}, nil
	}}
	windowsProvider := &scriptedProvider{id: "tju-ev17", sourceType: "windows_ssh", targets: []model.Identity{identity}, collect: func() ([]model.Snapshot, error) {
		return currentWindows, currentErr
	}}
	recorder := &rpcRecorder{}
	runner, closeRunner := newTestRunner(t, []provider.Provider{pveProvider, windowsProvider}, recorder)
	defer closeRunner()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.authorityGracePeriod = time.Minute

	if err := runner.Cycle(context.Background()); !errors.Is(err, windowsErr) {
		t.Fatalf("first cycle error = %v", err)
	}
	if methods, _ := recorder.snapshot(); len(methods) != 0 {
		t.Fatalf("first failed authority cycle uploaded %v", methods)
	}

	currentWindows, currentErr = []model.Snapshot{windows}, nil
	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	methods, infos := recorder.snapshot()
	if len(methods) != 2 || methods[0] != "agent.basicInfo" || methods[1] != "agent.report" {
		t.Fatalf("successful cycle methods = %v", methods)
	}
	if len(infos) != 1 || infos[0].DiskTotal != windows.BasicInfo.DiskTotal {
		t.Fatalf("basic info did not come from Windows: %#v", infos)
	}

	now = now.Add(20 * time.Second)
	currentWindows, currentErr = nil, windowsErr
	if err := runner.Cycle(context.Background()); !errors.Is(err, windowsErr) {
		t.Fatalf("cached cycle error = %v", err)
	}
	methods, _ = recorder.snapshot()
	if len(methods) != 3 || methods[2] != "agent.report" {
		t.Fatalf("recent cached authority was not reported without duplicate basic info: %v", methods)
	}

	now = now.Add(61 * time.Second)
	if err := runner.Cycle(context.Background()); !errors.Is(err, windowsErr) {
		t.Fatalf("expired cycle error = %v", err)
	}
	if methods, _ = recorder.snapshot(); len(methods) != 3 {
		t.Fatalf("expired authority unexpectedly uploaded: %v", methods)
	}
}

func TestBasicInfoUploadsAgainOnlyWhenContentChanges(t *testing.T) {
	identity := model.Identity{SourceType: "proxmox", SourceID: "tju-1", ExternalID: "qemu:107"}
	_, snapshot := authoritativeFixtures(identity)
	source := &scriptedProvider{id: "only", sourceType: "test", collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{snapshot}, nil
	}}
	recorder := &rpcRecorder{}
	runner, closeRunner := newTestRunner(t, []provider.Provider{source}, recorder)
	defer closeRunner()

	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot.BasicInfo.DiskTotal++
	snapshot.Report.Disk.Total++
	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	methods, infos := recorder.snapshot()
	wantMethods := []string{"agent.basicInfo", "agent.report", "agent.report", "agent.basicInfo", "agent.report"}
	if len(methods) != len(wantMethods) {
		t.Fatalf("methods = %v, want %v", methods, wantMethods)
	}
	for i := range methods {
		if methods[i] != wantMethods[i] {
			t.Fatalf("methods = %v, want %v", methods, wantMethods)
		}
	}
	if len(infos) != 2 || infos[1].DiskTotal != infos[0].DiskTotal+1 {
		t.Fatalf("basic info uploads = %#v", infos)
	}
}

func TestCycleReportsBridgeBuildAsClientVersion(t *testing.T) {
	oldVersion, oldCommit := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldVersion, oldCommit })
	buildinfo.Version, buildinfo.Commit = "v0.2.0", "1234567890"

	identity := model.Identity{SourceType: "proxmox", SourceID: "site-a", ExternalID: "qemu:107"}
	_, snapshot := authoritativeFixtures(identity)
	snapshot.Tags = map[string]string{"metrics_source": "windows_ssh"}
	snapshot.BasicInfo.Version = "Windows 11"
	source := &scriptedProvider{id: "only", sourceType: "test", collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{snapshot}, nil
	}}
	recorder := &rpcRecorder{}
	runner, closeRunner := newTestRunner(t, []provider.Provider{source}, recorder)
	defer closeRunner()

	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, infos := recorder.snapshot()
	if len(infos) != 1 {
		t.Fatalf("basic info uploads = %#v", infos)
	}
	if got, want := infos[0].Version, "komari-bridge v0.2.0 (1234567) / windows-ssh"; got != want {
		t.Fatalf("client version = %q, want %q", got, want)
	}
}

func TestDecorateTopologyPublishesSiteAndParentMetadata(t *testing.T) {
	snapshots := map[string]model.Snapshot{
		"pve": {
			Identity: model.Identity{SourceType: "proxmox", SourceID: "tianjin-1", ExternalID: "node:pve"},
			Name:     "tju-ev1", Group: "天津-1", ResourceType: "node",
			BasicInfo: model.BasicInfo{Virtualization: "pve"},
		},
		"vm": {
			Identity:         model.Identity{SourceType: "proxmox", SourceID: "tianjin-1", ExternalID: "qemu:107"},
			ParentExternalID: "node:pve", Name: "tju-ev17", Group: "天津-1", ResourceType: "qemu",
		},
		"wsl": {
			Identity:         model.Identity{SourceType: "windows_ssh", SourceID: "tju-ev17", ExternalID: "wsl:guid"},
			ParentExternalID: "qemu:107", Name: "tju-ev15", Group: "天津-1", ResourceType: "wsl",
		},
		"docker": {
			Identity:         model.Identity{SourceType: "docker", SourceID: "tju-ev10", ExternalID: "compose:komari:web:1"},
			ParentExternalID: "qemu:107", Name: "komari / web", Group: "天津-1", ResourceType: "docker_compose_container",
		},
	}

	decorateTopology(snapshots)
	for key, snapshot := range snapshots {
		if snapshot.BasicInfo.Group != "tju-ev1" {
			t.Errorf("%s group = %q, want tju-ev1", key, snapshot.BasicInfo.Group)
		}
		if !strings.Contains(snapshot.BasicInfo.Tags, "bridge_site=tju-ev1") || !strings.Contains(snapshot.BasicInfo.Tags, "bridge_external_id="+snapshot.Identity.ExternalID) {
			t.Errorf("%s tags = %q", key, snapshot.BasicInfo.Tags)
		}
	}
	if got := snapshots["vm"].BasicInfo.Tags; !strings.Contains(got, "bridge_parent_external_id=node:pve") || !strings.Contains(got, "bridge_resource_type=qemu") {
		t.Fatalf("VM tags = %q", got)
	}
	if got := snapshots["wsl"].BasicInfo.Tags; !strings.Contains(got, "bridge_parent_external_id=qemu:107") || !strings.Contains(got, "bridge_resource_type=wsl") {
		t.Fatalf("WSL tags = %q", got)
	}
	if got := snapshots["docker"].BasicInfo.Tags; !strings.Contains(got, "bridge_parent_external_id=qemu:107") || !strings.Contains(got, "bridge_resource_type=docker_compose_container") {
		t.Fatalf("Docker tags = %q", got)
	}
}

func TestInvalidAuthoritySnapshotUsesLastValidValue(t *testing.T) {
	identity := model.Identity{SourceType: "proxmox", SourceID: "site-a", ExternalID: "qemu:107"}
	pve, windows := authoritativeFixtures(identity)
	current := windows
	pveProvider := &scriptedProvider{id: "site-a", sourceType: "proxmox", collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{pve}, nil
	}}
	windowsProvider := &scriptedProvider{id: "windows-a", sourceType: "windows_ssh", targets: []model.Identity{identity}, collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{current}, nil
	}}
	recorder := &rpcRecorder{}
	runner, closeRunner := newTestRunner(t, []provider.Provider{pveProvider, windowsProvider}, recorder)
	defer closeRunner()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }

	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	current.Report.RAM.Used = current.Report.RAM.Total + 1
	now = now.Add(20 * time.Second)
	err := runner.Cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid authoritative snapshot") || !strings.Contains(err.Error(), "memory usage") {
		t.Fatalf("Cycle() error = %v", err)
	}
	methods, _ := recorder.snapshot()
	if len(methods) != 3 || methods[2] != "agent.report" {
		t.Fatalf("invalid authority did not reuse the last valid report: %v", methods)
	}
}

func TestInvalidDiscoveryMetricsDoNotRejectValidAuthority(t *testing.T) {
	identity := model.Identity{SourceType: "proxmox", SourceID: "site-a", ExternalID: "qemu:107"}
	pve, windows := authoritativeFixtures(identity)
	pve.Report.RAM.Used = pve.Report.RAM.Total + 1
	pveProvider := &scriptedProvider{id: "site-a", sourceType: "proxmox", collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{pve}, nil
	}}
	windowsProvider := &scriptedProvider{id: "windows-a", sourceType: "windows_ssh", targets: []model.Identity{identity}, collect: func() ([]model.Snapshot, error) {
		return []model.Snapshot{windows}, nil
	}}
	recorder := &rpcRecorder{}
	runner, closeRunner := newTestRunner(t, []provider.Provider{pveProvider, windowsProvider}, recorder)
	defer closeRunner()
	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatalf("valid authority was rejected because discovery metrics were invalid: %v", err)
	}
	methods, infos := recorder.snapshot()
	if len(methods) != 2 || len(infos) != 1 || infos[0].DiskTotal != windows.BasicInfo.DiskTotal {
		t.Fatalf("unexpected uploads: methods=%v infos=%#v", methods, infos)
	}
}

func TestValidateSnapshotRejectsMixedOrImpossibleTotals(t *testing.T) {
	_, valid := authoritativeFixtures(model.Identity{SourceType: "test", SourceID: "test", ExternalID: "host:test"})
	if err := validateSnapshot(valid); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*model.Snapshot)
		want   string
	}{
		{name: "mixed disk totals", mutate: func(s *model.Snapshot) { s.BasicInfo.DiskTotal-- }, want: "basic disk total"},
		{name: "memory exceeds total", mutate: func(s *model.Snapshot) { s.Report.RAM.Used = s.Report.RAM.Total + 1 }, want: "memory usage"},
		{name: "missing online memory", mutate: func(s *model.Snapshot) { s.BasicInfo.MemoryTotal, s.Report.RAM.Total, s.Report.RAM.Used = 0, 0, 0 }, want: "no memory total"},
		{name: "CPU over 100", mutate: func(s *model.Snapshot) { s.Report.CPU.Usage = 101 }, want: "CPU usage"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := valid
			test.mutate(&snapshot)
			if err := validateSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSnapshot() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMergeSnapshotsUsesGuestMetricsAndKeepsTopology(t *testing.T) {
	t.Parallel()
	identity := model.Identity{SourceType: "proxmox", SourceID: "site-a", ExternalID: "qemu:105"}
	pve := model.Snapshot{
		Identity: identity, Name: "gpu-a", Group: "Lab A", ResourceType: "qemu",
		ParentExternalID: "node:pve-a", Tags: map[string]string{"node": "pve-a"},
		BasicInfo: model.BasicInfo{OS: "Ubuntu", MemoryTotal: 108, Virtualization: "qemu"},
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
	if got.BasicInfo.Virtualization != "qemu" {
		t.Fatalf("discovered virtualization was not preserved: %#v", got.BasicInfo)
	}
	if got.Tags["node"] != "pve-a" || got.Tags["metrics_source"] != "ssh" {
		t.Fatalf("tags were not merged: %#v", got.Tags)
	}
}
