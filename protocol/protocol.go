// Package protocol defines the wire format between towamori-agent and the
// Towamori platform. See docs/protocol.md for the human-readable contract.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

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
	// LinkRev is the revision of the link targets this agent holds. It is
	// only sent by agents that measure links ("" while they have none yet);
	// the platform answers with a new list when it differs.
	LinkRev *string `json:"link_rev,omitempty"`
}

type ReportResponse struct {
	Accepted        int      `json:"accepted"`
	IntervalSeconds int      `json:"interval_seconds,omitempty"`
	HelloRequired   bool     `json:"hello_required,omitempty"`
	Links           *LinkSet `json:"links,omitempty"`
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
	// Links is present on the sample that follows a finished round of link
	// measurements (about once a minute), not on every sample.
	Links []LinkResult `json:"links,omitempty"`
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

// ---- links: how well this machine reaches Towamori's probe locations ----

const (
	// LinkDomain is the only domain the agent will measure. Targets must be
	// a subdomain of it, so a platform can never point the agent at a
	// third party's address.
	LinkDomain = "probe.towamori.com"

	// MaxLinkTargets bounds the list the platform may send.
	MaxLinkTargets = 16
	// MinLinkPort keeps targets out of the well-known service ports.
	MinLinkPort = 1024
)

// LinkTarget is one probe location the agent connects to: a plain TCP
// connection that is closed again at once, timed. Nothing is sent or read.
type LinkTarget struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// LinkSet is the platform's list of targets with a revision that changes
// whenever the list does.
type LinkSet struct {
	Rev     string       `json:"rev"`
	Targets []LinkTarget `json:"targets"`
}

// LinkResult is one round of measurements against one target. Times are
// milliseconds of connection setup (about one round trip); Min, Avg and Max
// cover the successful attempts only. Err names why every attempt failed and
// is empty when at least one succeeded.
type LinkResult struct {
	ID   string  `json:"id"`
	Sent int     `json:"sent"`
	OK   int     `json:"ok"`
	Min  float64 `json:"min,omitempty"`
	Avg  float64 `json:"avg,omitempty"`
	Max  float64 `json:"max,omitempty"`
	Err  string  `json:"err,omitempty"`
}

// Link failure classes.
const (
	LinkErrTimeout     = "timeout"
	LinkErrRefused     = "refused"
	LinkErrUnreachable = "unreachable"
	LinkErrDNS         = "dns"
	LinkErrBlocked     = "blocked"
	LinkErrOther       = "error"
)

// ValidID reports whether id is a short plain identifier (letters, digits,
// dash), which is all a probe location needs.
func ValidID(id string) bool {
	if id == "" || len(id) > 16 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// CheckLinkTarget refuses anything that is not a probe location of ours.
func CheckLinkTarget(t LinkTarget) error {
	if !ValidID(t.ID) {
		return fmt.Errorf("bad id %q", t.ID)
	}
	host := strings.ToLower(t.Host)
	if host != t.Host || !strings.HasSuffix(host, "."+LinkDomain) || len(host) > 253 {
		return fmt.Errorf("host %q is not under %s", t.Host, LinkDomain)
	}
	for _, r := range strings.TrimSuffix(host, "."+LinkDomain) {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return fmt.Errorf("host %q has odd characters", t.Host)
		}
	}
	if t.Port < MinLinkPort || t.Port > 65535 {
		return fmt.Errorf("port %d is outside %d-65535", t.Port, MinLinkPort)
	}
	return nil
}

// LinkRev is the revision of a list: a short hash of its sorted entries.
func LinkRev(targets []LinkTarget) string {
	lines := make([]string, len(targets))
	for i, t := range targets {
		lines[i] = fmt.Sprintf("%s|%s|%d", t.ID, t.Host, t.Port)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:6])
}

// ErrTooManyTargets is returned when a list is longer than MaxLinkTargets.
var ErrTooManyTargets = errors.New("too many link targets")
