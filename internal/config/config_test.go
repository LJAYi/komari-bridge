package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExpandsEnvironmentAndDefaults(t *testing.T) {
	t.Setenv("TEST_KOMARI_KEY", "123456789012")
	t.Setenv("TEST_PVE_TOKEN", "pve-secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: ${TEST_KOMARI_KEY}
providers:
  proxmox:
    - id: site-a
      endpoint: https://pve.example.com:8006
      token_id: bridge@pve!monitor
      token_secret: ${TEST_PVE_TOKEN}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Proxmox[0].TokenSecret != "pve-secret" {
		t.Fatal("environment variable was not expanded")
	}
	if cfg.Scheduler.Interval.Duration != 20*time.Second {
		t.Fatalf("default interval = %v", cfg.Scheduler.Interval.Duration)
	}
}

func TestRejectsIntervalAtPresenceTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: 123456789012
scheduler:
  interval: 35s
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "35s") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadsCapabilityOrientedProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: 123456789012
providers:
  docker:
    - id: engine-a
      attach_to:
        source_type: proxmox
        source_id: site-a
        external_id: qemu:100
  agentless_ssh:
    - id: appliance-a
      address: appliance-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
  slurm:
    - id: cluster-a
      address: cluster-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
  windows_wsl:
    - id: workstation-a
      address: workstation-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers.Docker) != 1 || cfg.Providers.Docker[0].Endpoint != "unix:///var/run/docker.sock" || len(cfg.Providers.AgentlessSSH) != 1 || len(cfg.Providers.Slurm) != 1 || len(cfg.Providers.WindowsWSL) != 1 {
		t.Fatalf("unexpected providers: %#v", cfg.Providers)
	}
}

func TestDockerRejectsPartialParentIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: 123456789012
providers:
  docker:
    - id: engine-a
      attach_to:
        external_id: qemu:100
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "attach_to") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestAgentlessSSHRejectsEmbeddedSlurm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: 123456789012
providers:
  agentless_ssh:
    - id: appliance-a
      address: appliance-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
      enable_slurm: true
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "providers.slurm") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestRejectsDuplicateMetricAuthorities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: 123456789012
providers:
  agentless_ssh:
    - id: linux-a
      address: linux-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
      attach_to: &target
        source_type: proxmox
        source_id: site-a
        external_id: qemu:105
  windows_ssh:
    - id: windows-a
      address: windows-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
      attach_to: *target
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "both own metrics") || !strings.Contains(err.Error(), "qemu:105") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestWindowsWSLParentDoesNotClaimHostMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
komari:
  endpoint: https://komari.example.com
  auto_discovery_key: 123456789012
providers:
  agentless_ssh:
    - id: linux-a
      address: linux-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
      attach_to: &target
        source_type: proxmox
        source_id: site-a
        external_id: qemu:105
  windows_wsl:
    - id: windows-a
      address: windows-a.example.internal:22
      user: monitor
      password: test-only
      insecure_ignore_host_key: true
      attach_to: *target
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}
