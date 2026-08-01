package windowsssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
)

type Provider struct {
	cfg        config.WindowsSSHConfig
	timeout    time.Duration
	sshConfig  *ssh.ClientConfig
	sourceType string
	emitHost   bool

	mu           sync.Mutex
	client       *ssh.Client
	conn         net.Conn
	previousNet  networkCounters
	previousAt   time.Time
	previousWSL  map[string]cpuCounters
	previousWNet map[string]networkCounters
	previousWAt  map[string]time.Time
	previousWGPU map[string]cachedGPUs
}

type cachedGPUs struct {
	devices []model.GPUDevice
	savedAt time.Time
}

const wslGPUGracePeriod = time.Minute

type cpuCounters struct {
	Total uint64 `json:"total"`
	Idle  uint64 `json:"idle"`
}

type networkCounters struct {
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

type collectorOutput struct {
	Windows windowsReport `json:"windows"`
	WSL     []wslReport   `json:"wsl"`
}

type windowsReport struct {
	CPUName          string            `json:"cpu_name"`
	CPUCores         int               `json:"cpu_cores"`
	CPUPhysicalCores int               `json:"cpu_physical_cores"`
	Arch             string            `json:"arch"`
	OS               string            `json:"os"`
	Kernel           string            `json:"kernel"`
	CPUUsage         float64           `json:"cpu_usage"`
	MemoryTotal      int64             `json:"memory_total"`
	MemoryFree       int64             `json:"memory_free"`
	DiskTotal        int64             `json:"disk_total"`
	DiskFree         int64             `json:"disk_free"`
	Disks            []model.DiskMount `json:"disks"`
	NetworkUp        uint64            `json:"network_up"`
	NetworkDown      uint64            `json:"network_down"`
	Uptime           int64             `json:"uptime"`
	Processes        int               `json:"processes"`
	GPUs             []model.GPUDevice `json:"gpus"`
	GPUOK            bool              `json:"gpu_ok"`
}

type wslReport struct {
	GUID     string   `json:"guid"`
	Name     string   `json:"name"`
	Version  int      `json:"version"`
	BasePath string   `json:"base_path"`
	Online   bool     `json:"online"`
	Data     *wslData `json:"data"`
}

type wslData struct {
	CPUName          string            `json:"cpu_name"`
	CPUCores         int               `json:"cpu_cores"`
	CPUPhysicalCores int               `json:"cpu_physical_cores"`
	Arch             string            `json:"arch"`
	OS               string            `json:"os"`
	Kernel           string            `json:"kernel"`
	CPU              cpuCounters       `json:"cpu"`
	Memory           map[string]uint64 `json:"memory"`
	Load             []float64         `json:"load"`
	DiskTotal        int64             `json:"disk_total"`
	DiskUsed         int64             `json:"disk_used"`
	Disks            []model.DiskMount `json:"disks"`
	Network          networkCounters   `json:"network"`
	Uptime           int64             `json:"uptime"`
	Processes        int               `json:"processes"`
	GPUs             []model.GPUDevice `json:"gpus"`
	GPUOK            bool              `json:"gpu_ok"`
}

func New(cfg config.WindowsSSHConfig, timeout time.Duration) (*Provider, error) {
	return newProvider(cfg, timeout, "windows_ssh", true)
}

// NewWSL creates a discovery-oriented provider. Windows host metrics belong to
// the official Komari Agent; this provider emits only WSL child resources.
func NewWSL(cfg config.WindowsSSHConfig, timeout time.Duration) (*Provider, error) {
	cfg.DiscoverWSL = true
	cfg.EnableNVIDIA = false
	return newProvider(cfg, timeout, "windows_wsl", false)
}

func newProvider(cfg config.WindowsSSHConfig, timeout time.Duration, sourceType string, emitHost bool) (*Provider, error) {
	auth, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	hostKeyCallback := ssh.InsecureIgnoreHostKey() //nolint:gosec // guarded by explicit configuration below
	if !cfg.InsecureIgnoreHostKey {
		want := cfg.HostKey
		hostKeyCallback = func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != want {
				return fmt.Errorf("SSH host key mismatch: got %s", got)
			}
			return nil
		}
	}
	return &Provider{
		cfg: cfg, timeout: timeout, sourceType: sourceType, emitHost: emitHost,
		sshConfig:   &ssh.ClientConfig{User: cfg.User, Auth: auth, HostKeyCallback: hostKeyCallback, Timeout: timeout},
		previousWSL: make(map[string]cpuCounters), previousWNet: make(map[string]networkCounters), previousWAt: make(map[string]time.Time),
		previousWGPU: make(map[string]cachedGPUs),
	}, nil
}

func authMethods(cfg config.WindowsSSHConfig) ([]ssh.AuthMethod, error) {
	keyData := []byte(cfg.PrivateKey)
	if cfg.PrivateKeyPath != "" {
		var err error
		keyData, err = os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read SSH private key: %w", err)
		}
	}
	var methods []ssh.AuthMethod
	if len(keyData) > 0 {
		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("parse SSH private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH authentication method configured")
	}
	return methods, nil
}

func (p *Provider) ID() string         { return p.cfg.ID }
func (p *Provider) SourceType() string { return p.sourceType }

func (p *Provider) MetricTargets() []model.Identity {
	if !p.emitHost || p.cfg.AttachTo.SourceType == "" {
		return nil
	}
	return []model.Identity{{
		SourceType: p.cfg.AttachTo.SourceType,
		SourceID:   p.cfg.AttachTo.SourceID,
		ExternalID: p.cfg.AttachTo.ExternalID,
	}}
}

func (p *Provider) Collect(ctx context.Context) ([]model.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	output, err := runWithReconnect(ctx, func() ([]byte, error) {
		return p.run(ctx, collectorScript(p.emitHost))
	}, p.closeLocked)
	if err != nil {
		p.closeLocked()
		return nil, err
	}
	raw, err := decodeCollectorOutput(output)
	if err != nil {
		text := strings.TrimSpace(string(output))
		if len(text) > 2000 {
			text = text[:2000]
		}
		return nil, fmt.Errorf("decode Windows SSH collector output: %w: %s", err, text)
	}
	if p.emitHost {
		if err := validateWindowsReport(raw.Windows); err != nil {
			return nil, fmt.Errorf("validate Windows SSH collector output: %w", err)
		}
	}
	now := time.Now().UTC()
	if p.cfg.EnableNVIDIA {
		for index := range raw.WSL {
			distro := &raw.WSL[index]
			if !distro.Online || distro.Data == nil {
				continue
			}
			if !p.stabilizeWSLGPU(distro, now) {
				// Do not let a failed GPU probe clear GPU metadata previously stored
				// by Komari, including immediately after a bridge restart.
				distro.Online, distro.Data = false, nil
			}
		}
	}
	winNet := networkCounters{Up: raw.Windows.NetworkUp, Down: raw.Windows.NetworkDown}
	netUp, netDown := rates(winNet, p.previousNet, now.Sub(p.previousAt))
	p.previousNet, p.previousAt = winNet, now

	windowsGPUs := raw.Windows.GPUs
	gpuOK := raw.Windows.GPUOK
	gpuSource := "windows"
	if p.cfg.EnableNVIDIA && (!gpuOK || len(windowsGPUs) == 0) {
		for _, distro := range raw.WSL {
			if distro.Online && distro.Data != nil && distro.Data.GPUOK && len(distro.Data.GPUs) > 0 {
				windowsGPUs = distro.Data.GPUs
				gpuOK = true
				gpuSource = "wsl:" + distro.Name
				break
			}
		}
	}
	if p.emitHost && p.cfg.EnableNVIDIA && !gpuOK {
		return nil, fmt.Errorf("validate Windows SSH collector output: NVIDIA GPU collection failed")
	}
	identity := model.Identity{SourceType: p.SourceType(), SourceID: p.cfg.ID, ExternalID: "host:" + p.cfg.ID}
	resourceType := "windows"
	if p.cfg.AttachTo.SourceType != "" {
		identity = model.Identity{SourceType: p.cfg.AttachTo.SourceType, SourceID: p.cfg.AttachTo.SourceID, ExternalID: p.cfg.AttachTo.ExternalID}
		resourceType = ""
	}
	winReport := model.Report{
		CPU:     model.CPUReport{Name: raw.Windows.CPUName, Cores: raw.Windows.CPUCores, Arch: raw.Windows.Arch, Usage: raw.Windows.CPUUsage},
		RAM:     model.Usage{Total: raw.Windows.MemoryTotal, Used: clampUsed(raw.Windows.MemoryTotal, raw.Windows.MemoryFree)},
		Disk:    model.Usage{Total: raw.Windows.DiskTotal, Used: clampUsed(raw.Windows.DiskTotal, raw.Windows.DiskFree)},
		Disks:   raw.Windows.Disks,
		Network: model.NetworkReport{Up: netUp, Down: netDown, TotalUp: int64(raw.Windows.NetworkUp), TotalDown: int64(raw.Windows.NetworkDown)},
		Uptime:  raw.Windows.Uptime, Process: raw.Windows.Processes,
	}
	if p.cfg.EnableNVIDIA && len(windowsGPUs) > 0 {
		winReport.GPU = gpuReport(windowsGPUs)
	}
	winTags := map[string]string{"metrics_source": "windows_ssh"}
	if gpuSource != "windows" {
		winTags["gpu_source"] = gpuSource
	}
	var snapshots []model.Snapshot
	if p.emitHost {
		snapshots = append(snapshots, model.Snapshot{
			Identity: identity, Name: firstNonEmpty(p.cfg.Name, p.cfg.ID), Group: p.cfg.Group, ResourceType: resourceType,
			Tags: winTags,
			BasicInfo: model.BasicInfo{
				CPUName: raw.Windows.CPUName, CPUCores: raw.Windows.CPUCores, CPUPhysicalCores: raw.Windows.CPUPhysicalCores,
				Arch: raw.Windows.Arch, OS: raw.Windows.OS, KernelVersion: raw.Windows.Kernel,
				MemoryTotal: raw.Windows.MemoryTotal, DiskTotal: raw.Windows.DiskTotal, GPUName: formatGPUName(windowsGPUs),
				Version: "komari-bridge/windows-ssh",
			},
			Report: winReport, CollectedAt: now, Online: true, Priority: 100,
		})
	}
	if p.cfg.DiscoverWSL {
		for _, distro := range raw.WSL {
			if distro.Online && distro.Data != nil {
				// GPU support is optional for an individual WSL distribution. A
				// failed WSL GPU probe must not invalidate otherwise sound host
				// metrics (nor the enclosing Windows host).
				if err := validateWSLData(*distro.Data, false); err != nil {
					distro.Online, distro.Data = false, nil
				}
			}
			snapshots = append(snapshots, p.wslSnapshot(distro, now))
		}
	}
	return snapshots, nil
}

func validateWindowsReport(raw windowsReport) error {
	if strings.TrimSpace(raw.CPUName) == "" || raw.CPUCores <= 0 {
		return fmt.Errorf("invalid CPU data")
	}
	if strings.TrimSpace(raw.Arch) == "" || strings.TrimSpace(raw.OS) == "" || strings.TrimSpace(raw.Kernel) == "" {
		return fmt.Errorf("missing operating system identity")
	}
	if raw.MemoryTotal <= 0 || raw.MemoryFree < 0 || raw.MemoryFree > raw.MemoryTotal {
		return fmt.Errorf("invalid memory usage: free=%d total=%d", raw.MemoryFree, raw.MemoryTotal)
	}
	if raw.DiskTotal <= 0 || raw.DiskFree < 0 || raw.DiskFree > raw.DiskTotal {
		return fmt.Errorf("invalid disk usage: free=%d total=%d", raw.DiskFree, raw.DiskTotal)
	}
	if raw.Uptime <= 0 {
		return fmt.Errorf("invalid uptime")
	}
	return nil
}

func validateWSLData(raw wslData, requireGPU bool) error {
	if strings.TrimSpace(raw.CPUName) == "" || raw.CPUCores <= 0 || raw.CPU.Total == 0 {
		return fmt.Errorf("invalid CPU data")
	}
	memTotal, ok := raw.Memory["MemTotal"]
	if !ok || memTotal == 0 || raw.Memory["MemAvailable"] > memTotal {
		return fmt.Errorf("invalid memory data")
	}
	if raw.DiskTotal <= 0 || raw.DiskUsed < 0 || raw.DiskUsed > raw.DiskTotal {
		return fmt.Errorf("invalid disk usage")
	}
	if strings.TrimSpace(raw.Arch) == "" || strings.TrimSpace(raw.OS) == "" || strings.TrimSpace(raw.Kernel) == "" || raw.Uptime <= 0 {
		return fmt.Errorf("missing operating system data")
	}
	if requireGPU && !raw.GPUOK {
		return fmt.Errorf("NVIDIA GPU collection failed")
	}
	return nil
}

func cloneGPUDevices(devices []model.GPUDevice) []model.GPUDevice {
	return append([]model.GPUDevice(nil), devices...)
}

func (p *Provider) stabilizeWSLGPU(distro *wslReport, now time.Time) bool {
	if distro.Data.GPUOK {
		p.previousWGPU[distro.GUID] = cachedGPUs{devices: cloneGPUDevices(distro.Data.GPUs), savedAt: now}
		return true
	}
	cached, ok := p.previousWGPU[distro.GUID]
	if !ok || now.Sub(cached.savedAt) > wslGPUGracePeriod {
		return false
	}
	distro.Data.GPUs = cloneGPUDevices(cached.devices)
	distro.Data.GPUOK = true
	return true
}

func runWithReconnect(ctx context.Context, run func() ([]byte, error), reconnect func()) ([]byte, error) {
	output, err := run()
	if err == nil {
		return output, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return nil, err
	}
	reconnect()
	return run()
}

func decodeCollectorOutput(output []byte) (collectorOutput, error) {
	var raw collectorOutput
	jsonStart := bytes.IndexByte(output, '{')
	if jsonStart < 0 {
		jsonStart = 0
	}
	err := json.NewDecoder(bytes.NewReader(output[jsonStart:])).Decode(&raw)
	return raw, err
}

func (p *Provider) wslSnapshot(raw wslReport, now time.Time) model.Snapshot {
	name := raw.Name
	if alias := p.cfg.WSLNames[raw.Name]; alias != "" {
		name = alias
	}
	metricsSource := "windows_wsl"
	version := "komari-bridge/windows-wsl"
	if p.emitHost {
		metricsSource = "windows_ssh_wsl"
		version = "komari-bridge/windows-ssh"
	}
	snapshot := model.Snapshot{
		Identity: model.Identity{SourceType: p.SourceType(), SourceID: p.cfg.ID, ExternalID: "wsl:" + strings.ToLower(raw.GUID)},
		Name:     name, Group: p.cfg.Group, ResourceType: "wsl", Online: raw.Online && raw.Data != nil, CollectedAt: now, Priority: 100,
		Tags:      map[string]string{"metrics_source": metricsSource, "distribution": raw.Name, "wsl_version": fmt.Sprint(raw.Version)},
		BasicInfo: model.BasicInfo{OS: fmt.Sprintf("WSL%d %s", raw.Version, raw.Name), Virtualization: "wsl", Version: version},
	}
	if p.cfg.AttachTo.ExternalID != "" {
		snapshot.ParentExternalID = p.cfg.AttachTo.ExternalID
	}
	if !snapshot.Online {
		return snapshot
	}
	data := raw.Data
	cpuUsage := counterPercent(data.CPU, p.previousWSL[raw.GUID])
	netUp, netDown := rates(data.Network, p.previousWNet[raw.GUID], now.Sub(p.previousWAt[raw.GUID]))
	p.previousWSL[raw.GUID], p.previousWNet[raw.GUID], p.previousWAt[raw.GUID] = data.CPU, data.Network, now
	memTotal := int64(data.Memory["MemTotal"] * 1024)
	memAvailable := int64(data.Memory["MemAvailable"] * 1024)
	swapTotal := int64(data.Memory["SwapTotal"] * 1024)
	swapFree := int64(data.Memory["SwapFree"] * 1024)
	snapshot.BasicInfo = model.BasicInfo{
		CPUName: data.CPUName, CPUCores: data.CPUCores, CPUPhysicalCores: data.CPUPhysicalCores,
		Arch: data.Arch, OS: data.OS, KernelVersion: data.Kernel,
		MemoryTotal: memTotal, SwapTotal: swapTotal, DiskTotal: data.DiskTotal,
		GPUName: formatGPUName(data.GPUs), Virtualization: "wsl", Version: version,
	}
	snapshot.Report = model.Report{
		CPU:     model.CPUReport{Name: data.CPUName, Cores: data.CPUCores, Arch: data.Arch, Usage: cpuUsage},
		RAM:     model.Usage{Total: memTotal, Used: clampUsed(memTotal, memAvailable)},
		Swap:    model.Usage{Total: swapTotal, Used: clampUsed(swapTotal, swapFree)},
		Disk:    model.Usage{Total: data.DiskTotal, Used: data.DiskUsed},
		Disks:   data.Disks,
		Network: model.NetworkReport{Up: netUp, Down: netDown, TotalUp: int64(data.Network.Up), TotalDown: int64(data.Network.Down)},
		Uptime:  data.Uptime, Process: data.Processes,
	}
	if len(data.Load) >= 3 {
		snapshot.Report.Load = model.LoadReport{Load1: data.Load[0], Load5: data.Load[1], Load15: data.Load[2]}
	}
	if p.cfg.EnableNVIDIA && len(data.GPUs) > 0 {
		snapshot.Report.GPU = gpuReport(data.GPUs)
	}
	return snapshot
}

func (p *Provider) run(ctx context.Context, script string) ([]byte, error) {
	client, err := p.clientLocked(ctx)
	if err != nil {
		return nil, err
	}
	if p.conn != nil {
		_ = p.conn.SetDeadline(time.Now().Add(p.timeout))
		defer p.conn.SetDeadline(time.Time{})
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()
	session.Stdin = strings.NewReader(script)
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := session.CombinedOutput(collectorBootstrapCommand())
		done <- result{output: output, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = session.Close()
		return nil, ctx.Err()
	case result := <-done:
		if result.err != nil {
			text := strings.TrimSpace(string(result.output))
			if len(text) > 2000 {
				text = text[:2000]
			}
			return nil, fmt.Errorf("run Windows SSH collector: %w: %s", result.err, text)
		}
		return result.output, nil
	}
}

func (p *Provider) clientLocked(ctx context.Context) (*ssh.Client, error) {
	if p.client != nil {
		if _, _, err := p.client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return p.client, nil
		}
		p.closeLocked()
	}
	dialer := net.Dialer{Timeout: p.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", p.cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("dial SSH: %w", err)
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, p.cfg.Address, p.sshConfig)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SSH handshake: %w", err)
	}
	p.conn, p.client = conn, ssh.NewClient(sshConn, channels, requests)
	return p.client, nil
}

func (p *Provider) closeLocked() {
	if p.client != nil {
		_ = p.client.Close()
	}
	p.client, p.conn = nil, nil
}

func counterPercent(current, previous cpuCounters) float64 {
	if previous.Total == 0 || current.Total <= previous.Total {
		return 0
	}
	deltaTotal, deltaIdle := current.Total-previous.Total, current.Idle-previous.Idle
	if deltaIdle > deltaTotal {
		return 0
	}
	return float64(deltaTotal-deltaIdle) * 100 / float64(deltaTotal)
}

func rates(current, previous networkCounters, elapsed time.Duration) (int64, int64) {
	if elapsed <= 0 || previous.Up == 0 || current.Up < previous.Up || current.Down < previous.Down {
		return 0, 0
	}
	return int64(float64(current.Up-previous.Up) / elapsed.Seconds()), int64(float64(current.Down-previous.Down) / elapsed.Seconds())
}

func clampUsed(total, free int64) int64 {
	if free < 0 || free > total {
		return 0
	}
	return total - free
}

func gpuReport(gpus []model.GPUDevice) *model.GPUReport {
	var total float64
	for _, gpu := range gpus {
		total += gpu.Utilization
	}
	return &model.GPUReport{Count: len(gpus), AverageUsage: total / float64(len(gpus)), DetailedInfo: gpus}
}

func formatGPUName(gpus []model.GPUDevice) string {
	counts := make(map[string]int)
	var order []string
	for _, gpu := range gpus {
		name := strings.TrimSpace(gpu.Name)
		if name == "" {
			continue
		}
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	var result []string
	for _, name := range order {
		if counts[name] > 1 {
			result = append(result, fmt.Sprintf("%s × %d", name, counts[name]))
		} else {
			result = append(result, name)
		}
	}
	return strings.Join(result, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
