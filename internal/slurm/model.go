package slurm

import "time"

type Partition struct {
	Name         string `json:"name"`
	Availability string `json:"availability"`
	Nodes        int    `json:"nodes"`
	State        string `json:"state"`
	CPUs         string `json:"cpus"`
	GRES         string `json:"gres"`
}

type Job struct {
	ID        string `json:"id"`
	Partition string `json:"partition"`
	User      string `json:"user"`
	State     string `json:"state"`
	Elapsed   string `json:"elapsed"`
	Nodes     int    `json:"nodes"`
	Reason    string `json:"reason"`
	GRES      string `json:"gres"`
}

type Snapshot struct {
	SourceID       string      `json:"source_id"`
	CollectedAt    time.Time   `json:"collected_at"`
	ControllerUp   bool        `json:"controller_up"`
	NodeDaemonUp   bool        `json:"node_daemon_up"`
	Partitions     []Partition `json:"partitions"`
	Jobs           []Job       `json:"jobs"`
	JobsRunning    int         `json:"jobs_running"`
	JobsPending    int         `json:"jobs_pending"`
	JobsOther      int         `json:"jobs_other"`
	GPUsConfigured int         `json:"gpus_configured"`
	GPUsAllocated  int         `json:"gpus_allocated"`
}
