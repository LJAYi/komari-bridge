package docker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
)

const maxDockerResponse = 32 << 20

type Provider struct {
	cfg       config.DockerConfig
	baseURL   string
	http      *http.Client
	collectMu sync.Mutex
	stateMu   sync.Mutex
	previous  map[string]networkSample
}

type networkSample struct {
	up, down uint64
	at       time.Time
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

type statsResponse struct {
	CPUStats    cpuStats             `json:"cpu_stats"`
	PreCPUStats cpuStats             `json:"precpu_stats"`
	MemoryStats memoryStats          `json:"memory_stats"`
	Networks    map[string]networkIO `json:"networks"`
	PidsStats   struct {
		Current int `json:"current"`
	} `json:"pids_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemCPUUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs     int    `json:"online_cpus"`
}

type memoryStats struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

type networkIO struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

type collectedStats struct {
	stats      statsResponse
	inspect    inspectResponse
	err        error
	inspectErr error
}

type inspectResponse struct {
	HostConfig struct {
		NanoCPUs   int64  `json:"NanoCpus"`
		CPUQuota   int64  `json:"CpuQuota"`
		CPUPeriod  int64  `json:"CpuPeriod"`
		CPUSetCPUs string `json:"CpusetCpus"`
	} `json:"HostConfig"`
	State struct {
		StartedAt string `json:"StartedAt"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
}

func New(cfg config.DockerConfig, timeout time.Duration) (*Provider, error) {
	baseURL, client, err := dockerHTTPClient(cfg, timeout)
	if err != nil {
		return nil, fmt.Errorf("docker %q: %w", cfg.ID, err)
	}
	return &Provider{cfg: cfg, baseURL: baseURL, http: client, previous: make(map[string]networkSample)}, nil
}

func (p *Provider) ID() string         { return p.cfg.ID }
func (p *Provider) SourceType() string { return "docker" }

func (p *Provider) Collect(ctx context.Context) ([]model.Snapshot, error) {
	p.collectMu.Lock()
	defer p.collectMu.Unlock()

	all := "0"
	if p.cfg.IncludeStopped {
		all = "1"
	}
	var containers []containerSummary
	if err := p.getJSON(ctx, "/containers/json?all="+all, &containers); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	containers = selectedContainers(containers, p.cfg.IncludeAll)
	sort.Slice(containers, func(i, j int) bool { return logicalIdentity(containers[i]) < logicalIdentity(containers[j]) })

	results := make([]collectedStats, len(containers))
	semaphore := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for index := range containers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index].err = ctx.Err()
				return
			}
			if containers[index].State == "running" {
				path := "/containers/" + url.PathEscape(containers[index].ID) + "/stats?stream=false&one-shot=true"
				results[index].err = p.getJSON(ctx, path, &results[index].stats)
			}
			inspectPath := "/containers/" + url.PathEscape(containers[index].ID) + "/json"
			results[index].inspectErr = p.getJSON(ctx, inspectPath, &results[index].inspect)
		}(index)
	}
	wg.Wait()

	now := time.Now().UTC()
	snapshots := make([]model.Snapshot, 0, len(containers))
	for index, container := range containers {
		snapshots = append(snapshots, p.snapshot(container, results[index], now))
	}
	return snapshots, nil
}

func selectedContainers(containers []containerSummary, includeAll bool) []containerSummary {
	if includeAll {
		return containers
	}
	selected := containers[:0]
	for _, container := range containers {
		if enabledLabel(container.Labels["komari.bridge.monitor"]) {
			selected = append(selected, container)
		}
	}
	return selected
}

func enabledLabel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

func (p *Provider) snapshot(container containerSummary, result collectedStats, now time.Time) model.Snapshot {
	externalID, name, resourceType, modeTags := workloadIdentity(container)
	tags := map[string]string{
		"source": "docker", "docker_engine": p.cfg.ID, "docker_container_id": shortID(container.ID),
		"docker_image": container.Image, "docker_state": container.State, "docker_status": container.Status,
	}
	for key, value := range modeTags {
		tags[key] = value
	}
	if result.inspectErr == nil {
		tags["docker_restart_count"] = strconv.Itoa(result.inspect.RestartCount)
		if result.inspect.State.Health != nil {
			tags["docker_health"] = result.inspect.State.Health.Status
		}
		if result.inspect.State.OOMKilled {
			tags["docker_oom_killed"] = "true"
		}
		if container.State != "running" {
			tags["docker_exit_code"] = strconv.Itoa(result.inspect.State.ExitCode)
		}
	}
	snapshot := model.Snapshot{
		Identity: model.Identity{SourceType: p.SourceType(), SourceID: p.cfg.ID, ExternalID: externalID},
		Name:     name, Group: p.cfg.Group, ResourceType: resourceType, Tags: tags,
		CollectedAt: now, Online: container.State == "running" && result.err == nil, Priority: 20,
		BasicInfo: model.BasicInfo{
			CPUName: "Docker workload", OS: "Container: " + container.Image,
			Virtualization: "docker", Version: "komari-bridge/docker",
		},
	}
	if p.cfg.AttachTo.ExternalID != "" {
		snapshot.ParentExternalID = p.cfg.AttachTo.ExternalID
	}
	if !snapshot.Online {
		return snapshot
	}

	stats := result.stats
	cores := effectiveCPUCores(stats, result.inspect)
	memoryTotal := signed(stats.MemoryStats.Limit)
	memoryUsed := signed(containerMemoryUsed(stats.MemoryStats))
	if memoryTotal <= 0 || memoryUsed < 0 || memoryUsed > memoryTotal {
		snapshot.Online = false
		return snapshot
	}
	networkUp, networkDown := networkTotals(stats.Networks)
	rateUp, rateDown := p.networkRates(externalID, networkUp, networkDown, now)
	uptime := containerUptime(result.inspect.State.StartedAt, now)
	snapshot.BasicInfo.CPUCores = cores
	snapshot.BasicInfo.MemoryTotal = memoryTotal
	snapshot.Report = model.Report{
		CPU: model.CPUReport{Cores: cores, Usage: normalizedCPU(stats, cores)},
		RAM: model.Usage{Total: memoryTotal, Used: memoryUsed},
		Network: model.NetworkReport{
			Up: rateUp, Down: rateDown, TotalUp: signed(networkUp), TotalDown: signed(networkDown),
		},
		Uptime: uptime, Process: stats.PidsStats.Current,
		Message: "Container metrics observed through the Docker Engine API.",
	}
	return snapshot
}

func workloadIdentity(container containerSummary) (externalID, name, resourceType string, tags map[string]string) {
	labels := container.Labels
	name = strings.TrimPrefix(first(container.Names), "/")
	if name == "" {
		name = shortID(container.ID)
	}
	tags = map[string]string{"docker_mode": "standalone"}

	if serviceID := labels["com.docker.swarm.service.id"]; serviceID != "" || labels["com.docker.swarm.service.name"] != "" {
		serviceName := firstNonEmpty(labels["com.docker.swarm.service.name"], serviceID, name)
		taskName := labels["com.docker.swarm.task.name"]
		slot := swarmSlot(taskName)
		stableTask := firstNonEmpty(slot, labels["com.docker.swarm.task.id"], shortID(container.ID))
		externalID = "swarm:" + escapeID(firstNonEmpty(serviceID, serviceName)) + ":" + escapeID(stableTask)
		name = serviceName
		if slot != "" {
			name += " #" + slot
		}
		resourceType = "docker_swarm_task"
		tags = map[string]string{
			"docker_mode": "swarm", "docker_swarm_service": serviceName,
			"docker_swarm_service_id": serviceID, "docker_swarm_task": taskName, "docker_swarm_slot": slot,
		}
		return
	}

	if project := labels["com.docker.compose.project"]; project != "" {
		service := firstNonEmpty(labels["com.docker.compose.service"], name)
		number := firstNonEmpty(labels["com.docker.compose.container-number"], "1")
		externalID = "compose:" + escapeID(project) + ":" + escapeID(service) + ":" + escapeID(number)
		name = project + " / " + service
		if number != "1" {
			name += " #" + number
		}
		resourceType = "docker_compose_container"
		tags = map[string]string{
			"docker_mode": "compose", "docker_compose_project": project,
			"docker_compose_service": service, "docker_compose_number": number,
		}
		return
	}

	externalID = "container:" + escapeID(name)
	resourceType = "docker_container"
	return
}

func logicalIdentity(container containerSummary) string {
	externalID, _, _, _ := workloadIdentity(container)
	return externalID
}

func normalizedCPU(stats statsResponse, effectiveCores int) float64 {
	cpuDelta := stats.CPUStats.CPUUsage.TotalUsage - min(stats.CPUStats.CPUUsage.TotalUsage, stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := stats.CPUStats.SystemCPUUsage - min(stats.CPUStats.SystemCPUUsage, stats.PreCPUStats.SystemCPUUsage)
	onlineCPUs := stats.CPUStats.OnlineCPUs
	if onlineCPUs <= 0 {
		onlineCPUs = stats.PreCPUStats.OnlineCPUs
	}
	if cpuDelta == 0 || systemDelta == 0 || onlineCPUs <= 0 || effectiveCores <= 0 {
		return 0
	}
	dockerPercent := float64(cpuDelta) / float64(systemDelta) * float64(onlineCPUs) * 100
	usage := dockerPercent / float64(effectiveCores)
	if usage > 100 {
		return 100
	}
	return usage
}

func effectiveCPUCores(stats statsResponse, inspect inspectResponse) int {
	hostCPUs := stats.CPUStats.OnlineCPUs
	if hostCPUs <= 0 {
		hostCPUs = stats.PreCPUStats.OnlineCPUs
	}
	if count := cpuSetCount(inspect.HostConfig.CPUSetCPUs); count > 0 {
		return count
	}
	quotaCores := float64(0)
	if inspect.HostConfig.NanoCPUs > 0 {
		quotaCores = float64(inspect.HostConfig.NanoCPUs) / 1e9
	} else if inspect.HostConfig.CPUQuota > 0 && inspect.HostConfig.CPUPeriod > 0 {
		quotaCores = float64(inspect.HostConfig.CPUQuota) / float64(inspect.HostConfig.CPUPeriod)
	}
	if quotaCores > 0 {
		cores := int(quotaCores)
		if float64(cores) < quotaCores {
			cores++
		}
		return max(1, cores)
	}
	return max(1, hostCPUs)
}

func cpuSetCount(value string) int {
	seen := make(map[int]struct{})
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		bounds := strings.SplitN(item, "-", 2)
		start, err := strconv.Atoi(bounds[0])
		if err != nil || start < 0 {
			continue
		}
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(bounds[1])
			if err != nil || end < start {
				continue
			}
		}
		for cpu := start; cpu <= end; cpu++ {
			seen[cpu] = struct{}{}
		}
	}
	return len(seen)
}

func containerUptime(startedAt string, now time.Time) int64 {
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil || started.After(now) {
		return 0
	}
	return int64(now.Sub(started).Seconds())
}

func containerMemoryUsed(memory memoryStats) uint64 {
	cache := memory.Stats["inactive_file"]
	if cache == 0 {
		cache = memory.Stats["cache"]
	}
	if memory.Usage >= cache {
		return memory.Usage - cache
	}
	return memory.Usage
}

func networkTotals(networks map[string]networkIO) (up, down uint64) {
	for name, network := range networks {
		if name == "lo" {
			continue
		}
		up += network.TxBytes
		down += network.RxBytes
	}
	return
}

func (p *Provider) networkRates(key string, up, down uint64, now time.Time) (int64, int64) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	previous, ok := p.previous[key]
	p.previous[key] = networkSample{up: up, down: down, at: now}
	if !ok || up < previous.up || down < previous.down {
		return 0, 0
	}
	elapsed := now.Sub(previous.at).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	return signed(uint64(float64(up-previous.up) / elapsed)), signed(uint64(float64(down-previous.down) / elapsed))
}

func (p *Provider) getJSON(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDockerResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func dockerHTTPClient(cfg config.DockerConfig, timeout time.Duration) (string, *http.Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return "", nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	baseURL := ""
	switch strings.ToLower(u.Scheme) {
	case "unix":
		if u.Path == "" {
			return "", nil, fmt.Errorf("unix endpoint has no socket path")
		}
		socket := filepath.Clean(u.Path)
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socket)
		}
		baseURL = "http://docker"
	case "tcp":
		if !cfg.InsecureAllowHTTP {
			return "", nil, fmt.Errorf("plain TCP requires insecure_allow_http: true; prefer a TLS-protected https endpoint")
		}
		baseURL = "http://" + u.Host
	case "http":
		if !cfg.InsecureAllowHTTP {
			return "", nil, fmt.Errorf("plain HTTP requires insecure_allow_http: true; prefer HTTPS")
		}
		baseURL = strings.TrimRight(cfg.Endpoint, "/")
	case "https":
		tlsConfig, err := dockerTLSConfig(cfg)
		if err != nil {
			return "", nil, err
		}
		transport.TLSClientConfig = tlsConfig
		baseURL = strings.TrimRight(cfg.Endpoint, "/")
	default:
		return "", nil, fmt.Errorf("unsupported endpoint scheme %q", u.Scheme)
	}
	return baseURL, &http.Client{Timeout: timeout, Transport: transport}, nil
}

func dockerTLSConfig(cfg config.DockerConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // Explicit private-engine option.
	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read Docker CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("Docker CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.TLSCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load Docker client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}

func swarmSlot(taskName string) string {
	parts := strings.Split(taskName, ".")
	if len(parts) < 3 {
		return ""
	}
	if _, err := strconv.Atoi(parts[len(parts)-2]); err != nil {
		return ""
	}
	return parts[len(parts)-2]
}

func escapeID(value string) string {
	return url.QueryEscape(strings.TrimSpace(value))
}

func shortID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func signed(value uint64) int64 {
	if value > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(value)
}
