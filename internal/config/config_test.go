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
	if len(cfg.Providers.AgentlessSSH) != 1 || len(cfg.Providers.Slurm) != 1 || len(cfg.Providers.WindowsWSL) != 1 {
		t.Fatalf("unexpected providers: %#v", cfg.Providers)
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
