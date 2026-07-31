package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	d.Duration = v
	return nil
}

type Config struct {
	Komari    KomariConfig    `yaml:"komari"`
	Database  DatabaseConfig  `yaml:"database"`
	HTTP      HTTPConfig      `yaml:"http"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Providers ProvidersConfig `yaml:"providers"`
}

type KomariConfig struct {
	Endpoint         string   `yaml:"endpoint"`
	AutoDiscoveryKey string   `yaml:"auto_discovery_key"`
	Timeout          Duration `yaml:"timeout"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type HTTPConfig struct {
	Listen string `yaml:"listen"`
	APIKey string `yaml:"api_key"`
}

type SchedulerConfig struct {
	Interval Duration `yaml:"interval"`
}

type ProvidersConfig struct {
	Proxmox    []ProxmoxConfig    `yaml:"proxmox"`
	LinuxSSH   []LinuxSSHConfig   `yaml:"linux_ssh"`
	WindowsSSH []WindowsSSHConfig `yaml:"windows_ssh"`
}

type ResourceOverride struct {
	Name        string `yaml:"name"`
	OS          string `yaml:"os"`
	Arch        string `yaml:"arch"`
	GuestMemory string `yaml:"guest_memory"`
}

type ProxmoxConfig struct {
	ID                 string                      `yaml:"id"`
	Endpoint           string                      `yaml:"endpoint"`
	TokenID            string                      `yaml:"token_id"`
	TokenSecret        string                      `yaml:"token_secret"`
	Group              string                      `yaml:"group"`
	InsecureSkipVerify bool                        `yaml:"insecure_skip_verify"`
	SkipStopped        bool                        `yaml:"skip_stopped"`
	Names              map[string]string           `yaml:"names"`
	Resources          map[string]ResourceOverride `yaml:"resources"`
}

type ResourceIdentity struct {
	SourceType string `yaml:"source_type"`
	SourceID   string `yaml:"source_id"`
	ExternalID string `yaml:"external_id"`
}

type LinuxSSHConfig struct {
	ID                    string           `yaml:"id"`
	Address               string           `yaml:"address"`
	User                  string           `yaml:"user"`
	PrivateKeyPath        string           `yaml:"private_key_path"`
	PrivateKey            string           `yaml:"private_key"`
	Password              string           `yaml:"password"`
	HostKey               string           `yaml:"host_key"`
	InsecureIgnoreHostKey bool             `yaml:"insecure_ignore_host_key"`
	Name                  string           `yaml:"name"`
	Group                 string           `yaml:"group"`
	OS                    string           `yaml:"os"`
	AttachTo              ResourceIdentity `yaml:"attach_to"`
	EnableNVIDIA          bool             `yaml:"enable_nvidia"`
	EnableSlurm           bool             `yaml:"enable_slurm"`
}

type WindowsSSHConfig struct {
	ID                    string            `yaml:"id"`
	Address               string            `yaml:"address"`
	User                  string            `yaml:"user"`
	PrivateKeyPath        string            `yaml:"private_key_path"`
	PrivateKey            string            `yaml:"private_key"`
	Password              string            `yaml:"password"`
	HostKey               string            `yaml:"host_key"`
	InsecureIgnoreHostKey bool              `yaml:"insecure_ignore_host_key"`
	Name                  string            `yaml:"name"`
	Group                 string            `yaml:"group"`
	AttachTo              ResourceIdentity  `yaml:"attach_to"`
	EnableNVIDIA          bool              `yaml:"enable_nvidia"`
	DiscoverWSL           bool              `yaml:"discover_wsl"`
	WSLNames              map[string]string `yaml:"wsl_names"`
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./data/komari-bridge.db"
	}
	if cfg.Komari.Timeout.Duration == 0 {
		cfg.Komari.Timeout.Duration = 10 * time.Second
	}
	if cfg.HTTP.Listen == "" {
		cfg.HTTP.Listen = "127.0.0.1:9090"
	}
	if cfg.Scheduler.Interval.Duration == 0 {
		cfg.Scheduler.Interval.Duration = 20 * time.Second
	}
}

func validate(cfg Config) error {
	if cfg.Komari.Endpoint == "" || cfg.Komari.AutoDiscoveryKey == "" {
		return fmt.Errorf("komari.endpoint and komari.auto_discovery_key are required")
	}
	if cfg.Scheduler.Interval.Duration >= 35*time.Second {
		return fmt.Errorf("scheduler.interval must be below Komari's 35s HTTP presence TTL")
	}
	for i, p := range cfg.Providers.Proxmox {
		if p.ID == "" || p.Endpoint == "" || p.TokenID == "" || p.TokenSecret == "" {
			return fmt.Errorf("providers.proxmox[%d]: id, endpoint, token_id and token_secret are required", i)
		}
		for externalID, resource := range p.Resources {
			if resource.GuestMemory != "" && !strings.EqualFold(resource.GuestMemory, "qga") {
				return fmt.Errorf("providers.proxmox[%d].resources[%q]: guest_memory must be qga", i, externalID)
			}
		}
	}
	for i, p := range cfg.Providers.LinuxSSH {
		if p.ID == "" || p.Address == "" || p.User == "" {
			return fmt.Errorf("providers.linux_ssh[%d]: id, address and user are required", i)
		}
		if p.PrivateKeyPath == "" && p.PrivateKey == "" && p.Password == "" {
			return fmt.Errorf("providers.linux_ssh[%d]: an SSH credential is required", i)
		}
		if !p.InsecureIgnoreHostKey && p.HostKey == "" {
			return fmt.Errorf("providers.linux_ssh[%d]: host_key is required unless insecure_ignore_host_key is true", i)
		}
		attached := p.AttachTo.SourceType != "" || p.AttachTo.SourceID != "" || p.AttachTo.ExternalID != ""
		if attached && (p.AttachTo.SourceType == "" || p.AttachTo.SourceID == "" || p.AttachTo.ExternalID == "") {
			return fmt.Errorf("providers.linux_ssh[%d]: attach_to requires source_type, source_id and external_id", i)
		}
	}
	for i, p := range cfg.Providers.WindowsSSH {
		if p.ID == "" || p.Address == "" || p.User == "" {
			return fmt.Errorf("providers.windows_ssh[%d]: id, address and user are required", i)
		}
		if p.PrivateKeyPath == "" && p.PrivateKey == "" && p.Password == "" {
			return fmt.Errorf("providers.windows_ssh[%d]: an SSH credential is required", i)
		}
		if !p.InsecureIgnoreHostKey && p.HostKey == "" {
			return fmt.Errorf("providers.windows_ssh[%d]: host_key is required unless insecure_ignore_host_key is true", i)
		}
		attached := p.AttachTo.SourceType != "" || p.AttachTo.SourceID != "" || p.AttachTo.ExternalID != ""
		if attached && (p.AttachTo.SourceType == "" || p.AttachTo.SourceID == "" || p.AttachTo.ExternalID == "") {
			return fmt.Errorf("providers.windows_ssh[%d]: attach_to requires source_type, source_id and external_id", i)
		}
	}
	return nil
}
