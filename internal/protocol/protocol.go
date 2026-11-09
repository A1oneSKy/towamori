// Package protocol defines the wire format between towamori-agent and the
// Towamori platform. See docs/protocol.md for the human-readable contract.
package protocol

// Version is the protocol version sent with every request. It only changes
// for breaking changes; new optional fields can be added to version 1.
const Version = 1

const (
	HelloPath  = "/v1/agent/hello"
	ReportPath = "/v1/agent/report"

	// HeaderProtocol carries Version on every request.
	HeaderProtocol = "X-Towamori-Protocol"

	// MinInterval and MaxInterval bound the reporting interval the platform
	// may ask for, in seconds.
	MinInterval = 5
	MaxInterval = 300
)

// AgentInfo identifies the agent build.
type AgentInfo struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// HostInfo describes the machine. It is sent once per hello.
type HostInfo struct {
	Hostname    string `json:"hostname"`
	MachineID   string `json:"machine_id,omitempty"`
	OSName      string `json:"os_name,omitempty"`
	OSVersion   string `json:"os_version,omitempty"`
	OSPretty    string `json:"os_pretty,omitempty"`
	Kernel      string `json:"kernel,omitempty"`
	CPUModel    string `json:"cpu_model,omitempty"`
	CPUCores    int    `json:"cpu_cores"`
	MemoryTotal uint64 `json:"memory_total,omitempty"`
	BootTime    int64  `json:"boot_time,omitempty"`
}

type HelloRequest struct {
	Protocol int       `json:"protocol"`
	Agent    AgentInfo `json:"agent"`
	Host     HostInfo  `json:"host"`
}

type HelloResponse struct {
	HostID          string `json:"host_id"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	ServerTime      int64  `json:"server_time,omitempty"`
}

type ReportRequest struct {
	Protocol int      `json:"protocol"`
	HostID   string   `json:"host_id,omitempty"`
	Samples  []Sample `json:"samples"`
}

type ReportResponse struct {
	Accepted        int  `json:"accepted"`
	IntervalSeconds int  `json:"interval_seconds,omitempty"`
	HelloRequired   bool `json:"hello_required,omitempty"`
}

// Sample is one point-in-time reading. Pointer fields are omitted when the
// value is not available yet (rates need two readings) or not supported on
// this operating system.
type Sample struct {
	TS     int64    `json:"ts"`
	Uptime int64    `json:"uptime,omitempty"`
	Procs  int      `json:"procs,omitempty"`
	CPU    *CPU     `json:"cpu,omitempty"`
	Load   *Load    `json:"load,omitempty"`
	Mem    *Mem     `json:"mem,omitempty"`
	Disks  []Disk   `json:"disks,omitempty"`
	DiskIO *DiskIO  `json:"disk_io,omitempty"`
	Net    *Network `json:"net,omitempty"`
}

// CPU percentages are shares (0-100) of total CPU time across all cores
// since the previous sample.
type CPU struct {
	Usage  float64 `json:"usage"`
	User   float64 `json:"user"`
	System float64 `json:"system"`
	IOWait float64 `json:"iowait"`
	Steal  float64 `json:"steal"`
}

type Load struct {
	L1  float64 `json:"l1"`
	L5  float64 `json:"l5"`
	L15 float64 `json:"l15"`
}

type Mem struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	Used      uint64 `json:"used"`
	SwapTotal uint64 `json:"swap_total"`
	SwapUsed  uint64 `json:"swap_used"`
}

type Disk struct {
	Mount       string `json:"mount"`
	FSType      string `json:"fstype"`
	Total       uint64 `json:"total"`
	Used        uint64 `json:"used"`
	Avail       uint64 `json:"avail"`
	InodesTotal uint64 `json:"inodes_total"`
	InodesUsed  uint64 `json:"inodes_used"`
}

type DiskIO struct {
	ReadBps  float64 `json:"read_bps"`
	WriteBps float64 `json:"write_bps"`
}

type Network struct {
	RxBps   float64 `json:"rx_bps"`
	TxBps   float64 `json:"tx_bps"`
	RxTotal uint64  `json:"rx_total"`
	TxTotal uint64  `json:"tx_total"`
	Ifaces  []Iface `json:"ifaces,omitempty"`
}

type Iface struct {
	Name  string  `json:"name"`
	RxBps float64 `json:"rx_bps"`
	TxBps float64 `json:"tx_bps"`
	// Tunnel marks VPN/tunnel interfaces (tun, wg, tailscale, …). They are
	// not included in Network totals, to avoid counting traffic twice.
	Tunnel bool `json:"tunnel,omitempty"`
}

// ClampInterval keeps a platform-requested interval inside the allowed range.
// Zero means "no change" and is returned unchanged.
func ClampInterval(seconds int) int {
	if seconds == 0 {
		return 0
	}
	if seconds < MinInterval {
		return MinInterval
	}
	if seconds > MaxInterval {
		return MaxInterval
	}
	return seconds
}
