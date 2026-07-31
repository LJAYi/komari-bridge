package linuxssh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
	"github.com/LJAYi/komari-bridge/internal/slurm"
)

type Provider struct {
	cfg        config.LinuxSSHConfig
	timeout    time.Duration
	sshConfig  *ssh.ClientConfig
	slurmStore *slurm.Store

	mu          sync.Mutex
	client      *ssh.Client
	conn        net.Conn
	previousCPU cpuCounters
	previousNet networkCounters
	previousAt  time.Time
}

type cpuCounters struct{ Total, Idle uint64 }
type networkCounters struct{ Up, Down uint64 }

func New(cfg config.LinuxSSHConfig, timeout time.Duration, slurmStore *slurm.Store) (*Provider, error) {
	auth, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	hostKeyCallback := ssh.InsecureIgnoreHostKey() //nolint:gosec // guarded by explicit configuration below
	if !cfg.InsecureIgnoreHostKey {
		want := cfg.HostKey
		hostKeyCallback = func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != want {
				return fmt.Errorf("SSH host key mismatch: got %s", ssh.FingerprintSHA256(key))
			}
			return nil
		}
	}
	return &Provider{
		cfg: cfg, timeout: timeout, slurmStore: slurmStore,
		sshConfig: &ssh.ClientConfig{
			User: cfg.User, Auth: auth, HostKeyCallback: hostKeyCallback, Timeout: timeout,
		},
	}, nil
}

func authMethods(cfg config.LinuxSSHConfig) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	keyData := []byte(cfg.PrivateKey)
	if cfg.PrivateKeyPath != "" {
		var err error
		keyData, err = os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read SSH private key: %w", err)
		}
	}
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
func (p *Provider) SourceType() string { return "linux_ssh" }

type remoteReport struct {
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
	Network          networkCounters   `json:"network"`
	TCP              int               `json:"tcp"`
	UDP              int               `json:"udp"`
	Uptime           int64             `json:"uptime"`
	Processes        int               `json:"processes"`
	GPUs             []model.GPUDevice `json:"gpus"`
	Slurm            remoteSlurm       `json:"slurm"`
}

type remoteSlurm struct {
	Available      bool              `json:"available"`
	ControllerUp   bool              `json:"controller_up"`
	NodeDaemonUp   bool              `json:"node_daemon_up"`
	Partitions     []slurm.Partition `json:"partitions"`
	Jobs           []slurm.Job       `json:"jobs"`
	GPUsConfigured int               `json:"gpus_configured"`
	GPUsAllocated  int               `json:"gpus_allocated"`
}

func (p *Provider) Collect(ctx context.Context) ([]model.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	output, err := p.run(ctx, collectorScript)
	if err != nil {
		p.closeLocked()
		return nil, err
	}
	var raw remoteReport
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("decode SSH collector output: %w", err)
	}
	now := time.Now().UTC()
	cpuUsage := counterPercent(raw.CPU.Total, raw.CPU.Idle, p.previousCPU.Total, p.previousCPU.Idle)
	netUp, netDown := rates(raw.Network, p.previousNet, now.Sub(p.previousAt))
	p.previousCPU, p.previousNet, p.previousAt = raw.CPU, raw.Network, now

	identity := model.Identity{SourceType: p.SourceType(), SourceID: p.cfg.ID, ExternalID: "host:" + p.cfg.ID}
	resourceType := "linux"
	if p.cfg.AttachTo.SourceType != "" {
		identity = model.Identity{
			SourceType: p.cfg.AttachTo.SourceType,
			SourceID:   p.cfg.AttachTo.SourceID,
			ExternalID: p.cfg.AttachTo.ExternalID,
		}
		resourceType = ""
	}
	osName := raw.OS
	if p.cfg.OS != "" {
		osName = p.cfg.OS
	}
	memTotal := int64(raw.Memory["MemTotal"] * 1024)
	memAvailable := int64(raw.Memory["MemAvailable"] * 1024)
	swapTotal := int64(raw.Memory["SwapTotal"] * 1024)
	swapFree := int64(raw.Memory["SwapFree"] * 1024)
	report := model.Report{
		CPU:         model.CPUReport{Name: raw.CPUName, Cores: raw.CPUCores, Arch: raw.Arch, Usage: cpuUsage},
		RAM:         model.Usage{Total: memTotal, Used: clampUsed(memTotal, memAvailable)},
		Swap:        model.Usage{Total: swapTotal, Used: clampUsed(swapTotal, swapFree)},
		Disk:        model.Usage{Total: raw.DiskTotal, Used: raw.DiskUsed},
		Network:     model.NetworkReport{Up: netUp, Down: netDown, TotalUp: int64(raw.Network.Up), TotalDown: int64(raw.Network.Down)},
		Connections: model.ConnectionsReport{TCP: raw.TCP, UDP: raw.UDP},
		Uptime:      raw.Uptime, Process: raw.Processes,
	}
	if len(raw.Load) >= 3 {
		report.Load = model.LoadReport{Load1: raw.Load[0], Load5: raw.Load[1], Load15: raw.Load[2]}
	}
	if p.cfg.EnableNVIDIA && len(raw.GPUs) > 0 {
		var total float64
		for _, gpu := range raw.GPUs {
			total += gpu.Utilization
		}
		report.GPU = &model.GPUReport{Count: len(raw.GPUs), AverageUsage: total / float64(len(raw.GPUs)), DetailedInfo: raw.GPUs}
	}
	if p.cfg.EnableSlurm && raw.Slurm.Available && p.slurmStore != nil {
		snapshot := slurm.Snapshot{
			SourceID: p.cfg.ID, CollectedAt: now,
			ControllerUp: raw.Slurm.ControllerUp, NodeDaemonUp: raw.Slurm.NodeDaemonUp,
			Partitions: raw.Slurm.Partitions, Jobs: raw.Slurm.Jobs,
			GPUsConfigured: raw.Slurm.GPUsConfigured, GPUsAllocated: raw.Slurm.GPUsAllocated,
		}
		for _, job := range snapshot.Jobs {
			switch strings.ToUpper(job.State) {
			case "RUNNING":
				snapshot.JobsRunning++
			case "PENDING":
				snapshot.JobsPending++
			default:
				snapshot.JobsOther++
			}
		}
		p.slurmStore.Set(p.cfg.ID, snapshot)
		report.Message = fmt.Sprintf("Slurm: %d running, %d pending; GPUs %d/%d allocated", snapshot.JobsRunning, snapshot.JobsPending, snapshot.GPUsAllocated, snapshot.GPUsConfigured)
	}
	name := firstNonEmpty(p.cfg.Name, p.cfg.ID)
	return []model.Snapshot{{
		Identity: identity, Name: name, Group: p.cfg.Group, ResourceType: resourceType,
		Tags: map[string]string{"metrics_source": "ssh"},
		BasicInfo: model.BasicInfo{
			CPUName: raw.CPUName, CPUCores: raw.CPUCores, CPUPhysicalCores: raw.CPUPhysicalCores,
			Arch: raw.Arch, OS: osName, KernelVersion: raw.Kernel,
			MemoryTotal: memTotal, SwapTotal: swapTotal, DiskTotal: raw.DiskTotal,
			GPUName:        formatGPUName(raw.GPUs),
			Virtualization: "qemu", Version: "komari-bridge/ssh",
		},
		Report: report, CollectedAt: now, Online: true, Priority: 100,
	}}, nil
}

func formatGPUName(gpus []model.GPUDevice) string {
	counts := make(map[string]int)
	order := make([]string, 0, len(gpus))
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
	result := make([]string, 0, len(order))
	for _, name := range order {
		if counts[name] > 1 {
			result = append(result, fmt.Sprintf("%s × %d", name, counts[name]))
		} else {
			result = append(result, name)
		}
	}
	return strings.Join(result, ", ")
}

func (p *Provider) run(ctx context.Context, command string) ([]byte, error) {
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
	session.Stdin = strings.NewReader(command)
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := session.Output("python3 -")
		done <- result{output: output, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = session.Close()
		return nil, ctx.Err()
	case result := <-done:
		if result.err != nil {
			return nil, fmt.Errorf("run SSH collector: %w", result.err)
		}
		return result.output, nil
	}
}

func (p *Provider) clientLocked(ctx context.Context) (*ssh.Client, error) {
	if p.client != nil {
		_, _, err := p.client.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
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
	p.conn = conn
	p.client = ssh.NewClient(sshConn, channels, requests)
	return p.client, nil
}

func (p *Provider) closeLocked() {
	if p.client != nil {
		_ = p.client.Close()
	}
	p.client, p.conn = nil, nil
}

func counterPercent(total, idle, previousTotal, previousIdle uint64) float64 {
	if previousTotal == 0 || total <= previousTotal {
		return 0
	}
	deltaTotal := total - previousTotal
	deltaIdle := idle - previousIdle
	if deltaIdle > deltaTotal {
		return 0
	}
	return float64(deltaTotal-deltaIdle) * 100 / float64(deltaTotal)
}

func rates(current, previous networkCounters, elapsed time.Duration) (int64, int64) {
	if elapsed <= 0 || previous.Up == 0 || current.Up < previous.Up || current.Down < previous.Down {
		return 0, 0
	}
	seconds := elapsed.Seconds()
	return int64(float64(current.Up-previous.Up) / seconds), int64(float64(current.Down-previous.Down) / seconds)
}

func clampUsed(total, free int64) int64 {
	if free < 0 || free > total {
		return 0
	}
	return total - free
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
