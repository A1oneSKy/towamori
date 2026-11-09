// Package collect reads host metrics. On Linux it reads /proc and calls
// statfs(2); it never executes programs, never opens network connections and
// never reads anything outside the files listed in docs/protocol.md.
package collect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/towamori/agent/protocol"
)

// ErrUnsupported is reported when metrics are requested on an operating
// system other than Linux. The agent still runs there, for development.
var ErrUnsupported = errors.New("host metrics are only collected on Linux; this OS reports host info only")

// fsStats is what statfs(2) tells us about one mounted filesystem.
type fsStats struct {
	total, free, avail, files, filesFree uint64
}

// counters are cumulative values; rates come from the difference between two
// readings.
type counters struct {
	at        time.Time
	cpu       cpuTimes
	haveCPU   bool
	diskRead  uint64
	diskWrite uint64
	haveDisk  bool
	ifaces    map[string]ifaceCounters
}

// Collector produces Samples. It keeps the previous counters to compute
// rates, so one Collector should be used for the life of the agent.
type Collector struct {
	root          string
	linux         bool
	statfs        func(path string) (fsStats, error)
	statfsTimeout time.Duration
	mountsFile    string // relative to root
	now           func() time.Time
	numCPU        func() int
	prev          *counters

	mu    sync.Mutex
	stuck map[string]bool // mount points whose statfs has not returned yet
}

// DefaultStatfsTimeout bounds how long one filesystem may take to answer.
const DefaultStatfsTimeout = 2 * time.Second

// New returns a Collector for this machine. hostRoot is "/" normally; when
// the agent runs in a container with the host's root filesystem mounted
// somewhere else (docker run -v /:/host:ro --pid=host --network=host
// --uts=host), pass that mount point. The mount table is then read from the
// host's init process, because /proc/self/mounts would describe the
// container.
func New(hostRoot string) *Collector {
	if hostRoot == "" {
		hostRoot = "/"
	}
	mountsFile := "proc/self/mounts"
	if filepath.Clean(hostRoot) != "/" {
		mountsFile = "proc/1/mounts"
	}
	return &Collector{
		root:          hostRoot,
		mountsFile:    mountsFile,
		linux:         runtime.GOOS == "linux",
		statfs:        statfs,
		statfsTimeout: DefaultStatfsTimeout,
		now:           time.Now,
		numCPU:        runtime.NumCPU,
	}
}

// statfsBounded calls statfs with a deadline. statfs on a FUSE filesystem can
// block forever when the daemon behind it hangs, and the call cannot be
// cancelled. So a mount that does not answer in time is skipped, and while
// that call is still stuck the mount is skipped again instead of starting
// another blocked call.
func (c *Collector) statfsBounded(path string) (fsStats, error) {
	c.mu.Lock()
	if c.stuck == nil {
		c.stuck = map[string]bool{}
	}
	if c.stuck[path] {
		c.mu.Unlock()
		return fsStats{}, fmt.Errorf("statfs %s: still not answering, skipped", path)
	}
	c.mu.Unlock()

	type result struct {
		st  fsStats
		err error
	}
	ch := make(chan result, 1)
	go func() {
		st, err := c.statfs(path)
		ch <- result{st, err} // buffered: never blocks, even if nobody reads
		c.mu.Lock()
		delete(c.stuck, path)
		c.mu.Unlock()
	}()

	timeout := c.statfsTimeout
	if timeout <= 0 {
		timeout = DefaultStatfsTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.st, r.err
	case <-timer.C:
	}
	// Re-check under the lock: if the call finished at the last moment, use
	// it; otherwise mark the mount as stuck. The goroutine clears the mark
	// after this, because it takes the lock only after sending.
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case r := <-ch:
		return r.st, r.err
	default:
	}
	c.stuck[path] = true
	return fsStats{}, fmt.Errorf("statfs %s: no answer within %s, skipped", path, timeout)
}

func (c *Collector) read(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(c.root, rel))
	return string(b), err
}

// Host describes the machine for the hello request.
func (c *Collector) Host() protocol.HostInfo {
	h := protocol.HostInfo{CPUCores: c.numCPU()}
	if s, err := c.read("proc/sys/kernel/hostname"); err == nil && strings.TrimSpace(s) != "" {
		h.Hostname = strings.TrimSpace(s)
	} else if s, err := c.read("etc/hostname"); err == nil && strings.TrimSpace(s) != "" {
		h.Hostname = strings.TrimSpace(s)
	} else if n, err := os.Hostname(); err == nil {
		h.Hostname = n
	}
	if !c.linux {
		h.OSName = runtime.GOOS
		return h
	}
	if s, err := c.read("etc/machine-id"); err == nil && strings.TrimSpace(s) != "" {
		sum := sha256.Sum256([]byte("towamori:" + strings.TrimSpace(s)))
		h.MachineID = hex.EncodeToString(sum[:])[:32]
	}
	osr, err := c.read("etc/os-release")
	if err != nil {
		osr, _ = c.read("usr/lib/os-release")
	}
	r := parseOSRelease(osr)
	h.OSName, h.OSVersion, h.OSPretty = r.name, r.version, r.pretty
	if s, err := c.read("proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(s)
	}
	if s, err := c.read("proc/cpuinfo"); err == nil {
		h.CPUModel = parseCPUModel(s)
	}
	if s, err := c.read("proc/meminfo"); err == nil {
		if m, err := parseMeminfo(s); err == nil {
			h.MemoryTotal = m.total
		}
	}
	if s, err := c.read("proc/stat"); err == nil {
		if _, bt, err := parseStat(s); err == nil {
			h.BootTime = bt
		}
	}
	return h
}

// Sample takes one reading. Sections that cannot be read are left out and
// the reasons are returned, so `towamori-agent --once` can show them.
func (c *Collector) Sample() (protocol.Sample, []error) {
	now := c.now()
	s := protocol.Sample{TS: now.Unix()}
	if !c.linux {
		return s, []error{ErrUnsupported}
	}
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	cur := &counters{at: now}

	if d, err := c.read("proc/uptime"); err == nil {
		up, err := parseUptime(d)
		note(err)
		s.Uptime = up
	} else {
		note(err)
	}

	if d, err := c.read("proc/stat"); err == nil {
		cpu, _, err := parseStat(d)
		note(err)
		if err == nil {
			cur.cpu, cur.haveCPU = cpu, true
		}
	} else {
		note(err)
	}

	if d, err := c.read("proc/loadavg"); err == nil {
		l, err := parseLoadAvg(d)
		note(err)
		if err == nil {
			s.Load = &protocol.Load{L1: l.l1, L5: l.l5, L15: l.l15}
		}
	} else {
		note(err)
	}

	if d, err := c.read("proc/meminfo"); err == nil {
		m, err := parseMeminfo(d)
		note(err)
		if err == nil {
			swapUsed := uint64(0)
			if m.swapTotal > m.swapFree {
				swapUsed = m.swapTotal - m.swapFree
			}
			s.Mem = &protocol.Mem{
				Total:     m.total,
				Available: m.available,
				Used:      m.total - m.available,
				SwapTotal: m.swapTotal,
				SwapUsed:  swapUsed,
			}
		}
	} else {
		note(err)
	}

	if d, err := c.read("proc/diskstats"); err == nil {
		r, w, err := parseDiskstats(d)
		note(err)
		if err == nil {
			cur.diskRead, cur.diskWrite, cur.haveDisk = r, w, true
		}
	} else {
		note(err)
	}

	if d, err := c.read("proc/net/dev"); err == nil {
		ifs, err := parseNetDev(d)
		note(err)
		if err == nil {
			cur.ifaces = ifs
		}
	} else {
		note(err)
	}

	if d, err := c.read(c.mountsFile); err == nil {
		s.Disks = c.disks(parseMounts(d), note)
	} else {
		note(err)
	}

	s.Procs = c.countProcs()

	c.addRates(&s, cur)
	c.prev = cur
	return s, errs
}

func (c *Collector) disks(mounts []mount, note func(error)) []protocol.Disk {
	var out []protocol.Disk
	for _, m := range mounts {
		st, err := c.statfsBounded(filepath.Join(c.root, m.point))
		if err != nil {
			note(err)
			continue
		}
		used := uint64(0)
		if st.total > st.free {
			used = st.total - st.free
		}
		inodesUsed := uint64(0)
		if st.files > st.filesFree {
			inodesUsed = st.files - st.filesFree
		}
		out = append(out, protocol.Disk{
			Mount:       m.point,
			FSType:      m.fstype,
			Total:       st.total,
			Used:        used,
			Avail:       st.avail,
			InodesTotal: st.files,
			InodesUsed:  inodesUsed,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Mount == "/" {
			return out[j].Mount != "/"
		}
		if out[j].Mount == "/" {
			return false
		}
		return out[i].Mount < out[j].Mount
	})
	return out
}

func (c *Collector) countProcs() int {
	entries, err := os.ReadDir(filepath.Join(c.root, "proc"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && isDigits(e.Name()) {
			n++
		}
	}
	return n
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// delta returns cur-prev, or 0 when the counter went backwards (reboot or
// wrap-around), so a reset never shows up as a huge spike.
func delta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func (c *Collector) addRates(s *protocol.Sample, cur *counters) {
	p := c.prev
	var net *protocol.Network
	if cur.ifaces != nil {
		net = &protocol.Network{}
		for name, ic := range cur.ifaces {
			if isTunnel(name) {
				continue // already counted on the physical interface
			}
			net.RxTotal += ic.rx
			net.TxTotal += ic.tx
		}
	}
	if p == nil {
		s.Net = net
		return
	}
	secs := cur.at.Sub(p.at).Seconds()
	if secs <= 0 {
		s.Net = net
		return
	}

	if cur.haveCPU && p.haveCPU {
		total := float64(delta(cur.cpu.total(), p.cpu.total()))
		if total > 0 {
			pct := func(v uint64) float64 { return round2(float64(v) / total * 100) }
			idle := delta(cur.cpu.idle, p.cpu.idle)
			iowait := delta(cur.cpu.iowait, p.cpu.iowait)
			busy := uint64(0)
			if t := delta(cur.cpu.total(), p.cpu.total()); t > idle+iowait {
				busy = t - idle - iowait
			}
			s.CPU = &protocol.CPU{
				Usage:  pct(busy),
				User:   pct(delta(cur.cpu.user, p.cpu.user) + delta(cur.cpu.nice, p.cpu.nice)),
				System: pct(delta(cur.cpu.system, p.cpu.system) + delta(cur.cpu.irq, p.cpu.irq) + delta(cur.cpu.softirq, p.cpu.softirq)),
				IOWait: pct(iowait),
				Steal:  pct(delta(cur.cpu.steal, p.cpu.steal)),
			}
		}
	}

	if cur.haveDisk && p.haveDisk {
		s.DiskIO = &protocol.DiskIO{
			ReadBps:  round2(float64(delta(cur.diskRead, p.diskRead)) / secs),
			WriteBps: round2(float64(delta(cur.diskWrite, p.diskWrite)) / secs),
		}
	}

	if net != nil && p.ifaces != nil {
		names := make([]string, 0, len(cur.ifaces))
		for n := range cur.ifaces {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ic := cur.ifaces[n]
			pc, ok := p.ifaces[n]
			if !ok {
				continue // interface appeared since the last reading
			}
			rx := round2(float64(delta(ic.rx, pc.rx)) / secs)
			tx := round2(float64(delta(ic.tx, pc.tx)) / secs)
			tunnel := isTunnel(n)
			if !tunnel {
				net.RxBps += rx
				net.TxBps += tx
			}
			net.Ifaces = append(net.Ifaces, protocol.Iface{Name: n, RxBps: rx, TxBps: tx, Tunnel: tunnel})
		}
		net.RxBps, net.TxBps = round2(net.RxBps), round2(net.TxBps)
	}
	s.Net = net
}
