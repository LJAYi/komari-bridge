package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
)

type Provider struct {
	id          string
	endpoint    string
	tokenID     string
	secret      string
	group       string
	skipStopped bool
	names       map[string]string
	resources   map[string]config.ResourceOverride
	http        *http.Client
	cacheMu     sync.RWMutex
	nodeStatus  map[string]nodeStatusEnrichment
	guestOS     map[string]guestOSEnrichment
	guestMemory map[string]cachedGuestMemory
	guestDisk   map[string]cachedGuestDisk
	network     map[string]networkSample
}

const guestEnrichmentGracePeriod = time.Minute

type cachedGuestMemory struct {
	memory  guestMemory
	savedAt time.Time
}

type cachedGuestDisk struct {
	disk    guestDisk
	savedAt time.Time
}

type networkSample struct {
	up      int64
	down    int64
	savedAt time.Time
}

type nodeStatusEnrichment struct {
	CPUName          string
	CPUCores         int
	CPUPhysicalCores int
	Arch             string
	OS               string
	KernelVersion    string
	Version          string
}

type guestOSEnrichment struct {
	OS            string
	Arch          string
	KernelVersion string
}

func New(cfg config.ProxmoxConfig, timeout time.Duration) (*Provider, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid Proxmox endpoint %q", cfg.Endpoint)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // Explicit per-source option for private PVE certificates.
	return &Provider{
		id: cfg.ID, endpoint: u.String(), tokenID: cfg.TokenID, secret: cfg.TokenSecret,
		group: cfg.Group, skipStopped: cfg.SkipStopped, names: cfg.Names, resources: cfg.Resources,
		http:        &http.Client{Timeout: timeout, Transport: transport},
		nodeStatus:  make(map[string]nodeStatusEnrichment),
		guestOS:     make(map[string]guestOSEnrichment),
		guestMemory: make(map[string]cachedGuestMemory),
		guestDisk:   make(map[string]cachedGuestDisk),
		network:     make(map[string]networkSample),
	}, nil
}

func (p *Provider) ID() string         { return p.id }
func (p *Provider) SourceType() string { return "proxmox" }

type apiResponse struct {
	Data []resource `json:"data"`
}

type nodeStatusResponse struct {
	Data struct {
		CPUInfo struct {
			Model   string `json:"model"`
			CPUs    int    `json:"cpus"`
			Cores   int    `json:"cores"`
			Sockets int    `json:"sockets"`
		} `json:"cpuinfo"`
		PVEVersion string `json:"pveversion"`
		Kernel     string `json:"kversion"`
	} `json:"data"`
}

type guestOSResponse struct {
	Data struct {
		Result struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			PrettyName    string `json:"pretty-name"`
			Version       string `json:"version"`
			VersionID     string `json:"version-id"`
			KernelRelease string `json:"kernel-release"`
			KernelVersion string `json:"kernel-version"`
			Machine       string `json:"machine"`
		} `json:"result"`
	} `json:"data"`
}

type guestExecResponse struct {
	Data struct {
		PID int `json:"pid"`
	} `json:"data"`
}

type guestExecStatusResponse struct {
	Data struct {
		Exited    int    `json:"exited"`
		ExitCode  int    `json:"exitcode"`
		OutData   string `json:"out-data"`
		Truncated int    `json:"out-truncated"`
	} `json:"data"`
}

type guestMemory struct {
	MemoryTotal     int64
	MemoryAvailable int64
	SwapTotal       int64
	SwapFree        int64
}

type guestFilesystemResponse struct {
	Data struct {
		Result []guestFilesystem `json:"result"`
	} `json:"data"`
}

type guestFilesystem struct {
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type"`
	TotalBytes int64  `json:"total-bytes"`
	UsedBytes  int64  `json:"used-bytes"`
	Disks      []struct {
		Device string `json:"dev"`
		Serial string `json:"serial"`
	} `json:"disk"`
}

type guestDisk struct {
	Total  int64
	Used   int64
	Mounts []model.DiskMount
}

type resource struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Node     string  `json:"node"`
	Name     string  `json:"name"`
	VMID     int     `json:"vmid"`
	Status   string  `json:"status"`
	CPU      float64 `json:"cpu"`
	MaxCPU   float64 `json:"maxcpu"`
	Mem      int64   `json:"mem"`
	MaxMem   int64   `json:"maxmem"`
	Disk     int64   `json:"disk"`
	MaxDisk  int64   `json:"maxdisk"`
	NetIn    int64   `json:"netin"`
	NetOut   int64   `json:"netout"`
	Uptime   int64   `json:"uptime"`
	Template int     `json:"template"`
}

func (p *Provider) Collect(ctx context.Context) ([]model.Snapshot, error) {
	var payload apiResponse
	if err := p.getJSON(ctx, "/api2/json/cluster/resources", &payload); err != nil {
		return nil, fmt.Errorf("cluster resources: %w", err)
	}

	now := time.Now().UTC()
	snapshots := make([]model.Snapshot, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.Template != 0 || (item.Type != "node" && item.Type != "qemu" && item.Type != "lxc") {
			continue
		}
		if p.skipStopped && item.Status != "online" && item.Status != "running" {
			continue
		}
		snapshot := p.snapshot(item, now)
		if item.Type == "node" {
			var status nodeStatusResponse
			node := firstNonEmpty(item.Node, item.Name, strings.TrimPrefix(item.ID, "node/"))
			if err := p.getJSON(ctx, "/api2/json/nodes/"+url.PathEscape(node)+"/status", &status); err == nil && validNodeStatus(status) {
				enrichment := nodeStatusEnrichment{
					CPUName:  cpuDisplayName(status.Data.CPUInfo.Model, status.Data.CPUInfo.Sockets),
					CPUCores: status.Data.CPUInfo.CPUs, CPUPhysicalCores: status.Data.CPUInfo.Cores,
					// Proxmox VE is distributed for the amd64 platform. Keep the
					// Komari spelling consistent with QGA and Linux collectors.
					Arch: "x86_64", OS: "Proxmox VE", KernelVersion: status.Data.Kernel,
					Version: status.Data.PVEVersion,
				}
				p.storeNodeStatus(node, enrichment)
				applyNodeStatus(&snapshot, enrichment)
			} else if enrichment, ok := p.loadNodeStatus(node); ok {
				applyNodeStatus(&snapshot, enrichment)
			} else {
				// Node status is the only source of the real CPU identity and
				// architecture. Do not publish a generic first sample if this
				// required enrichment is temporarily unavailable.
				snapshot.Online = false
			}
		}
		if item.Type == "qemu" && item.Status == "running" {
			var osInfo guestOSResponse
			path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/get-osinfo", url.PathEscape(item.Node), item.VMID)
			if err := p.getJSON(ctx, path, &osInfo); err == nil && validGuestOS(osInfo) {
				enrichment := guestOSEnrichment{
					OS:   firstNonEmpty(osInfo.Data.Result.PrettyName, osInfo.Data.Result.Name),
					Arch: osInfo.Data.Result.Machine, KernelVersion: osInfo.Data.Result.KernelRelease,
				}
				p.storeGuestOS(snapshot.Identity.ExternalID, enrichment)
				applyGuestOS(&snapshot, enrichment)
			} else if enrichment, ok := p.loadGuestOS(snapshot.Identity.ExternalID); ok {
				applyGuestOS(&snapshot, enrichment)
			}

			var filesystems guestFilesystemResponse
			fsPath := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/get-fsinfo", url.PathEscape(item.Node), item.VMID)
			if err := p.getJSON(ctx, fsPath, &filesystems); err == nil {
				if disk, err := summarizeGuestFilesystems(filesystems.Data.Result); err == nil {
					p.storeGuestDisk(snapshot.Identity.ExternalID, disk)
					applyGuestDisk(&snapshot, disk, false)
					snapshot.Tags["disk_source"] = "qga_get_fsinfo"
				} else if disk, ok := p.loadGuestDisk(snapshot.Identity.ExternalID); ok {
					applyGuestDisk(&snapshot, disk, true)
					snapshot.Tags["disk_source"] = "qga_get_fsinfo_cached"
				}
			} else if disk, ok := p.loadGuestDisk(snapshot.Identity.ExternalID); ok {
				applyGuestDisk(&snapshot, disk, true)
				snapshot.Tags["disk_source"] = "qga_get_fsinfo_cached"
			}
		}
		if override, ok := p.resources[snapshot.Identity.ExternalID]; ok {
			if item.Type == "qemu" && item.Status == "running" && strings.EqualFold(override.GuestMemory, "qga") {
				if memory, err := p.collectGuestMemory(ctx, item.Node, item.VMID); err == nil {
					p.storeGuestMemory(snapshot.Identity.ExternalID, memory)
					applyGuestMemory(&snapshot, memory, false)
					snapshot.Tags["memory_source"] = "qga_proc_meminfo"
				} else if memory, ok := p.loadGuestMemory(snapshot.Identity.ExternalID); ok {
					applyGuestMemory(&snapshot, memory, true)
					snapshot.Tags["memory_source"] = "qga_proc_meminfo_cached"
				} else {
					// guest_memory:qga is an explicit accuracy contract. Until QGA
					// succeeds there is no compatible memory observation to report;
					// mark this snapshot offline rather than mixing in PVE process memory.
					snapshot.Online = false
					snapshot.BasicInfo.MemoryTotal = 0
					snapshot.BasicInfo.SwapTotal = 0
					snapshot.Report.RAM = model.Usage{}
					snapshot.Report.Swap = model.Usage{}
					snapshot.Tags["memory_source"] = "qga_unavailable"
				}
			}
			if override.Name != "" {
				snapshot.Name = override.Name
			}
			if override.OS != "" {
				snapshot.BasicInfo.OS = override.OS
			}
			if override.Arch != "" {
				snapshot.BasicInfo.Arch = override.Arch
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func (p *Provider) collectGuestMemory(ctx context.Context, node string, vmid int) (guestMemory, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/exec", url.PathEscape(node), vmid)
	form := url.Values{"command": {"/bin/cat", "/proc/meminfo"}}
	var started guestExecResponse
	if err := p.postFormJSON(ctx, path, form, &started); err != nil {
		return guestMemory{}, fmt.Errorf("start QGA memory command: %w", err)
	}
	if started.Data.PID <= 0 {
		return guestMemory{}, fmt.Errorf("start QGA memory command: missing pid")
	}
	statusPath := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/exec-status?pid=%d", url.PathEscape(node), vmid, started.Data.PID)
	for attempt := 0; attempt < 10; attempt++ {
		var status guestExecStatusResponse
		if err := p.getJSON(ctx, statusPath, &status); err != nil {
			return guestMemory{}, fmt.Errorf("read QGA memory command: %w", err)
		}
		if status.Data.Exited != 0 {
			if status.Data.ExitCode != 0 {
				return guestMemory{}, fmt.Errorf("QGA memory command exited with status %d", status.Data.ExitCode)
			}
			if status.Data.Truncated != 0 {
				return guestMemory{}, fmt.Errorf("QGA memory output was truncated")
			}
			return parseGuestMemory(status.Data.OutData)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return guestMemory{}, ctx.Err()
		case <-timer.C:
		}
	}
	return guestMemory{}, fmt.Errorf("QGA memory command timed out")
}

func parseGuestMemory(contents string) (guestMemory, error) {
	values := make(map[string]int64)
	present := make(map[string]bool)
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		switch key {
		case "MemTotal", "MemAvailable", "SwapTotal", "SwapFree":
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || value < 0 {
				return guestMemory{}, fmt.Errorf("invalid %s value", key)
			}
			values[key] = value * 1024
			present[key] = true
		}
	}
	if !present["MemTotal"] || !present["MemAvailable"] || values["MemTotal"] <= 0 {
		return guestMemory{}, fmt.Errorf("QGA memory output is missing required fields")
	}
	if values["MemAvailable"] > values["MemTotal"] {
		return guestMemory{}, fmt.Errorf("QGA memory available exceeds total")
	}
	if present["SwapTotal"] != present["SwapFree"] || values["SwapFree"] > values["SwapTotal"] {
		return guestMemory{}, fmt.Errorf("QGA swap output is inconsistent")
	}
	return guestMemory{
		MemoryTotal: values["MemTotal"], MemoryAvailable: values["MemAvailable"],
		SwapTotal: values["SwapTotal"], SwapFree: values["SwapFree"],
	}, nil
}

func usedMemory(total, available int64) int64 {
	if total <= 0 || available < 0 || available > total {
		return 0
	}
	return total - available
}

func summarizeGuestFilesystems(filesystems []guestFilesystem) (guestDisk, error) {
	seen := make(map[string]struct{})
	var disk guestDisk
	for _, filesystem := range filesystems {
		if filesystem.TotalBytes <= 0 || filesystem.UsedBytes < 0 || filesystem.UsedBytes > filesystem.TotalBytes {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(filesystem.Type)) {
		case "squashfs", "iso9660", "udf", "tmpfs", "devtmpfs":
			continue
		}
		key := firstNonEmpty(strings.TrimSpace(filesystem.Name), strings.TrimSpace(filesystem.Mountpoint))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		disk.Total += filesystem.TotalBytes
		disk.Used += filesystem.UsedBytes
		device := ""
		if len(filesystem.Disks) > 0 {
			device = firstNonEmpty(filesystem.Disks[0].Device, filesystem.Disks[0].Serial)
		}
		disk.Mounts = append(disk.Mounts, model.DiskMount{
			Name: filesystem.Name, Mountpoint: firstNonEmpty(filesystem.Mountpoint, filesystem.Name),
			Filesystem: filesystem.Type, Device: device,
			Total: filesystem.TotalBytes, Used: filesystem.UsedBytes,
		})
	}
	if disk.Total <= 0 || disk.Used < 0 || disk.Used > disk.Total {
		return guestDisk{}, fmt.Errorf("QGA filesystem response contains no usable filesystems")
	}
	return disk, nil
}

func applyNodeStatus(snapshot *model.Snapshot, enrichment nodeStatusEnrichment) {
	snapshot.BasicInfo.CPUName = enrichment.CPUName
	snapshot.BasicInfo.CPUCores = enrichment.CPUCores
	snapshot.BasicInfo.CPUPhysicalCores = enrichment.CPUPhysicalCores
	snapshot.BasicInfo.Arch = enrichment.Arch
	snapshot.BasicInfo.OS = enrichment.OS
	snapshot.BasicInfo.KernelVersion = enrichment.KernelVersion
	snapshot.BasicInfo.Version = enrichment.Version
}

func validNodeStatus(status nodeStatusResponse) bool {
	return strings.TrimSpace(status.Data.CPUInfo.Model) != "" &&
		status.Data.CPUInfo.CPUs > 0 && status.Data.CPUInfo.Cores > 0 &&
		status.Data.Kernel != "" && status.Data.PVEVersion != ""
}

func validGuestOS(info guestOSResponse) bool {
	result := info.Data.Result
	return strings.TrimSpace(firstNonEmpty(result.PrettyName, result.Name)) != "" &&
		strings.TrimSpace(result.Machine) != "" && strings.TrimSpace(result.KernelRelease) != ""
}

func applyGuestOS(snapshot *model.Snapshot, enrichment guestOSEnrichment) {
	snapshot.BasicInfo.OS = enrichment.OS
	snapshot.BasicInfo.Arch = enrichment.Arch
	snapshot.BasicInfo.KernelVersion = enrichment.KernelVersion
}

func applyGuestMemory(snapshot *model.Snapshot, memory guestMemory, cached bool) {
	snapshot.BasicInfo.MemoryTotal = memory.MemoryTotal
	snapshot.BasicInfo.SwapTotal = memory.SwapTotal
	snapshot.Report.RAM = model.Usage{Total: memory.MemoryTotal, Used: usedMemory(memory.MemoryTotal, memory.MemoryAvailable)}
	snapshot.Report.Swap = model.Usage{Total: memory.SwapTotal, Used: usedMemory(memory.SwapTotal, memory.SwapFree)}
	snapshot.Report.Message = "Guest memory from /proc/meminfo via the QEMU Guest Agent; remaining metrics observed by Proxmox."
	if cached {
		snapshot.Report.Message = "Last known guest memory from /proc/meminfo via the QEMU Guest Agent; remaining metrics observed by Proxmox."
	}
}

func applyGuestDisk(snapshot *model.Snapshot, disk guestDisk, cached bool) {
	snapshot.BasicInfo.DiskTotal = disk.Total
	snapshot.Report.Disk = model.Usage{Total: disk.Total, Used: disk.Used}
	snapshot.Report.Disks = append([]model.DiskMount(nil), disk.Mounts...)
	snapshot.Report.Message = "Guest filesystem usage from the QEMU Guest Agent; remaining metrics observed by Proxmox."
	if cached {
		snapshot.Report.Message = "Last known guest filesystem usage from the QEMU Guest Agent; remaining metrics observed by Proxmox."
	}
}

func (p *Provider) storeNodeStatus(key string, enrichment nodeStatusEnrichment) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.nodeStatus[key] = enrichment
}

func (p *Provider) loadNodeStatus(key string) (nodeStatusEnrichment, bool) {
	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	enrichment, ok := p.nodeStatus[key]
	return enrichment, ok
}

func (p *Provider) storeGuestOS(key string, enrichment guestOSEnrichment) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.guestOS[key] = enrichment
}

func (p *Provider) loadGuestOS(key string) (guestOSEnrichment, bool) {
	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	enrichment, ok := p.guestOS[key]
	return enrichment, ok
}

func (p *Provider) storeGuestMemory(key string, memory guestMemory) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.guestMemory[key] = cachedGuestMemory{memory: memory, savedAt: time.Now()}
}

func (p *Provider) loadGuestMemory(key string) (guestMemory, bool) {
	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	cached, ok := p.guestMemory[key]
	if !ok || time.Since(cached.savedAt) > guestEnrichmentGracePeriod {
		return guestMemory{}, false
	}
	return cached.memory, true
}

func (p *Provider) storeGuestDisk(key string, disk guestDisk) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.guestDisk[key] = cachedGuestDisk{disk: disk, savedAt: time.Now()}
}

func (p *Provider) loadGuestDisk(key string) (guestDisk, bool) {
	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	cached, ok := p.guestDisk[key]
	if !ok || time.Since(cached.savedAt) > guestEnrichmentGracePeriod {
		return guestDisk{}, false
	}
	return cached.disk, true
}

func (p *Provider) networkRates(key string, up, down int64, now time.Time) (int64, int64) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	previous, ok := p.network[key]
	p.network[key] = networkSample{up: up, down: down, savedAt: now}
	if !ok || up < previous.up || down < previous.down {
		return 0, 0
	}
	elapsed := now.Sub(previous.savedAt).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	return int64(float64(up-previous.up) / elapsed), int64(float64(down-previous.down) / elapsed)
}

func cpuDisplayName(model string, sockets int) string {
	model = strings.TrimSpace(model)
	if model != "" && sockets > 1 {
		return fmt.Sprintf("%s × %d", model, sockets)
	}
	return model
}

func (p *Provider) getJSON(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+path, nil)
	if err != nil {
		return err
	}
	return p.doJSON(req, dst)
}

func (p *Provider) postFormJSON(ctx context.Context, path string, form url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return p.doJSON(req, dst)
}

func (p *Provider) doJSON(req *http.Request, dst any) error {
	req.Header.Set("Authorization", "PVEAPIToken="+p.tokenID+"="+p.secret)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (p *Provider) snapshot(item resource, now time.Time) model.Snapshot {
	externalID := item.Type + ":" + strconv.Itoa(item.VMID)
	parentID := "node:" + item.Node
	name := item.Name
	osName := "Proxmox " + strings.ToUpper(item.Type) + " guest (external metrics)"
	virtualization := item.Type
	if item.Type == "node" {
		externalID = "node:" + firstNonEmpty(item.Node, item.Name, strings.TrimPrefix(item.ID, "node/"))
		parentID = ""
		name = strings.TrimPrefix(externalID, "node:")
		osName = "Proxmox VE"
		virtualization = "pve"
	}
	if alias := p.names[externalID]; alias != "" {
		name = alias
	}
	if name == "" {
		name = externalID
	}
	maxCPU := int(item.MaxCPU)
	if maxCPU < 0 {
		maxCPU = 0
	}
	diskTotal, diskUsed := item.MaxDisk, item.Disk
	if item.Type == "qemu" {
		// PVE exposes allocated virtual-disk capacity for QEMU, not guest
		// filesystem usage. Reporting that capacity with a zero/host-side used
		// value creates a misleading 0% gauge, so leave the guest disk metric
		// unavailable until a guest-side authority enriches the snapshot.
		diskTotal, diskUsed = 0, 0
	}
	netUp, netDown := p.networkRates(externalID, item.NetOut, item.NetIn, now)
	return model.Snapshot{
		Identity:         model.Identity{SourceType: p.SourceType(), SourceID: p.id, ExternalID: externalID},
		ParentExternalID: parentID,
		Name:             name,
		ResourceType:     item.Type,
		Group:            p.group,
		Tags: map[string]string{
			"source": "proxmox", "resource_type": item.Type,
		},
		BasicInfo: model.BasicInfo{
			CPUName: "PVE observed CPU", CPUCores: maxCPU, OS: osName,
			MemoryTotal: item.MaxMem, DiskTotal: diskTotal,
			Virtualization: virtualization, Version: "komari-bridge",
		},
		Report: model.Report{
			CPU:     model.CPUReport{Cores: maxCPU, Usage: item.CPU * 100},
			RAM:     model.Usage{Total: item.MaxMem, Used: item.Mem},
			Disk:    model.Usage{Total: diskTotal, Used: diskUsed},
			Network: model.NetworkReport{Up: netUp, Down: netDown, TotalUp: item.NetOut, TotalDown: item.NetIn},
			Uptime:  item.Uptime,
			Message: "Metrics observed by Proxmox; guest filesystem and process metrics are unavailable.",
		},
		CollectedAt: now,
		Online:      item.Status == "online" || item.Status == "running",
		Priority:    10,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
