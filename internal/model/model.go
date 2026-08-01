package model

import "time"

// Identity is the stable provider-side identity of a discovered resource.
// Komari's UUID is deliberately not used here because it is server-generated.
type Identity struct {
	SourceType string
	SourceID   string
	ExternalID string
}

func (i Identity) Valid() bool {
	return i.SourceType != "" && i.SourceID != "" && i.ExternalID != ""
}

type BasicInfo struct {
	CPUName          string `json:"cpu_name"`
	CPUCores         int    `json:"cpu_cores"`
	CPUPhysicalCores int    `json:"cpu_physical_cores"`
	Arch             string `json:"arch"`
	OS               string `json:"os"`
	KernelVersion    string `json:"kernel_version"`
	IPv4             string `json:"ipv4"`
	IPv6             string `json:"ipv6"`
	MemoryTotal      int64  `json:"mem_total"`
	SwapTotal        int64  `json:"swap_total"`
	DiskTotal        int64  `json:"disk_total"`
	GPUName          string `json:"gpu_name"`
	Virtualization   string `json:"virtualization"`
	Version          string `json:"version"`
	Group            string `json:"group,omitempty"`
	Tags             string `json:"tags,omitempty"`
}

type Usage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

// DiskMount is an extension field carried by komari-bridge reports. Current
// Komari releases ignore unknown report fields; keeping the shape explicit here
// makes the bridge wire-compatible with a future server-side mountpoint API.
type DiskMount struct {
	Name       string `json:"name,omitempty"`
	Mountpoint string `json:"mountpoint"`
	Filesystem string `json:"filesystem,omitempty"`
	Device     string `json:"device,omitempty"`
	Total      int64  `json:"total"`
	Used       int64  `json:"used"`
}

type CPUReport struct {
	Name  string  `json:"name,omitempty"`
	Cores int     `json:"cores,omitempty"`
	Arch  string  `json:"arch,omitempty"`
	Usage float64 `json:"usage,omitempty"`
}

type LoadReport struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

type NetworkReport struct {
	Up        int64 `json:"up"`
	Down      int64 `json:"down"`
	TotalUp   int64 `json:"totalUp"`
	TotalDown int64 `json:"totalDown"`
}

type ConnectionsReport struct {
	TCP int `json:"tcp"`
	UDP int `json:"udp"`
}

type GPUDevice struct {
	Name        string  `json:"name"`
	MemoryTotal int64   `json:"memory_total"`
	MemoryUsed  int64   `json:"memory_used"`
	Utilization float64 `json:"utilization"`
	Temperature int     `json:"temperature"`
}

type GPUReport struct {
	Count        int         `json:"count"`
	AverageUsage float64     `json:"average_usage"`
	DetailedInfo []GPUDevice `json:"detailed_info"`
}

// Report matches Komari's v1 metric payload carried by the v2 JSON-RPC API.
type Report struct {
	CPU         CPUReport         `json:"cpu"`
	RAM         Usage             `json:"ram"`
	Swap        Usage             `json:"swap"`
	Load        LoadReport        `json:"load"`
	Disk        Usage             `json:"disk"`
	Disks       []DiskMount       `json:"disks,omitempty"`
	Network     NetworkReport     `json:"network"`
	Connections ConnectionsReport `json:"connections"`
	GPU         *GPUReport        `json:"gpu,omitempty"`
	Uptime      int64             `json:"uptime"`
	Process     int               `json:"process"`
	Message     string            `json:"message"`
}

type Snapshot struct {
	Identity         Identity
	ParentExternalID string
	Name             string
	ResourceType     string
	Group            string
	Tags             map[string]string
	BasicInfo        BasicInfo
	Report           Report
	CollectedAt      time.Time
	Online           bool
	// Priority controls whole-payload enrichment when multiple providers
	// describe the same canonical resource. PVE discovery uses a low priority;
	// guest-side SSH/agent observations use a higher priority.
	Priority int
}

type Binding struct {
	Identity   Identity
	KomariUUID string
	Token      string
}
