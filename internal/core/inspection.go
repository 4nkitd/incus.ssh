package core

import (
	"context"
	"time"
)

type AppMetrics struct {
	SampledAt      time.Time `json:"sampled_at"`
	CPUUsageNS     *int64    `json:"cpu_usage_ns"`
	MemoryBytes    *int64    `json:"memory_bytes"`
	DiskBytes      *int64    `json:"disk_bytes"`
	NetworkRXBytes *int64    `json:"network_rx_bytes"`
	NetworkTXBytes *int64    `json:"network_tx_bytes"`
	Processes      *int64    `json:"processes"`
}

type AppDetails struct {
	Instance                  Instance   `json:"instance"`
	Metrics                   AppMetrics `json:"metrics"`
	EnvironmentNames          []string   `json:"environment_names"`
	EnvironmentPendingRestart bool       `json:"environment_pending_restart"`
}

type EnvironmentUpdate struct {
	Values map[string]*string `json:"values"`
}

type AppInspector interface {
	InspectApp(context.Context, Principal, string) (AppDetails, error)
	UpdateEnvironment(context.Context, Principal, string, EnvironmentUpdate) error
}
