package collect

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/towamori/agent/internal/protocol"
)

// A small Ubuntu VPS, read twice ten seconds apart.
const (
	stat1 = `cpu  10000 500 3000 80000 400 0 100 200 0 0
cpu0 5000 250 1500 40000 200 0 50 100 0 0
cpu1 5000 250 1500 40000 200 0 50 100 0 0
intr 123456 0 0
ctxt 987654
btime 1794100000
processes 12345
procs_running 1
procs_blocked 0
`
	// +60 user, +20 system, +900 idle, +10 iowait, +5 softirq, +5 steal = 1000 ticks
	stat2 = `cpu  10060 500 3020 80900 410 0 105 205 0 0
btime 1794100000
`
	meminfo = `MemTotal:        2013804 kB
MemFree:          212340 kB
MemAvailable:    1258240 kB
Buffers:           58000 kB
Cached:           900000 kB
SwapCached:            0 kB
SwapTotal:       1048572 kB
SwapFree:         948572 kB
`
	netdev1 = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 5000000   50000    0    0    0     0          0         0  5000000   50000    0    0    0     0       0          0
  eth0: 100000000  80000    0    0    0     0          0         0 20000000   60000    0    0    0     0       0          0
docker0: 3000000  2000    0    0    0     0          0         0  4000000   3000    0    0    0     0       0          0
veth1a2b: 1000  10    0    0    0     0          0         0  2000   20    0    0    0     0       0          0
tailscale0: 7000000  900    0    0    0     0          0         0  8000000   900    0    0    0     0       0          0
`
	netdev2 = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9000000   50000    0    0    0     0          0         0  9000000   50000    0    0    0     0       0          0
  eth0: 100020480  80100    0    0    0     0          0         0 20010240   60100    0    0    0     0       0          0
docker0: 9000000  2000    0    0    0     0          0         0  9000000   3000    0    0    0     0       0          0
tailscale0: 7010000  950    0    0    0     0          0         0  8005000   950    0    0    0     0       0          0
`
	diskstats1 = `   7       0 loop0 100 0 2000 50 0 0 0 0 0 60 50 0 0 0 0 0 0
 252       0 vda 5000 100 400000 3000 8000 200 600000 9000 0 7000 12000 0 0 0 0 0 0
 252       1 vda1 4900 100 390000 2900 7900 200 590000 8900 0 6900 11800 0 0 0 0 0 0
 253       0 dm-0 100 0 1000 10 100 0 1000 10 0 20 20 0 0 0 0 0 0
`
	// vda wrote 800 more sectors (409600 bytes); partitions and dm-0 must not add to it.
	diskstats2 = `   7       0 loop0 100 0 9000 50 0 0 0 0 0 60 50 0 0 0 0 0 0
 252       0 vda 5000 100 400000 3000 8100 200 600800 9000 0 7000 12000 0 0 0 0 0 0
 252       1 vda1 4900 100 390000 2900 8000 200 590800 8900 0 6900 11800 0 0 0 0 0 0
 253       0 dm-0 100 0 1000 10 900 0 9000 10 0 20 20 0 0 0 0 0 0
`
	mounts = `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
/dev/vda1 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
/dev/vda15 /boot/efi vfat rw,relatime 0 0
/dev/vda1 /var/lib/docker ext4 rw,relatime 0 0
overlay /var/lib/docker/overlay2/abc/merged overlay rw 0 0
/dev/loop0 /snap/core22/1234 squashfs ro 0 0
/dev/sdb /mnt/my\040data xfs rw 0 0
/dev/sdc1 /mnt/data fuseblk rw,user_id=0,group_id=0,allow_other 0 0
`
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newTestCollector(t *testing.T) (*Collector, string, *time.Time) {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"proc/stat":                 stat1,
		"proc/meminfo":              meminfo,
		"proc/loadavg":              "0.12 0.08 0.05 1/245 12345\n",
		"proc/net/dev":              netdev1,
		"proc/diskstats":            diskstats1,
		"proc/self/mounts":          mounts,
		"proc/uptime":               "100010.55 190000.00\n",
		"proc/sys/kernel/hostname":  "web-1\n",
		"proc/sys/kernel/osrelease": "6.8.0-45-generic\n",
		"proc/cpuinfo":              "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD EPYC 7763 64-Core Processor\n",
		"proc/1/status":             "Name: systemd\n",
		"proc/42/status":            "Name: sshd\n",
		"etc/machine-id":            "0123456789abcdef0123456789abcdef\n",
		"etc/os-release":            "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nID=ubuntu\n",
	})
	now := time.Unix(1794200000, 0)
	disks := map[string]fsStats{
		"/":            {total: 40 << 30, free: 28 << 30, avail: 26 << 30, files: 2621440, filesFree: 2437940},
		"/boot/efi":    {total: 104 << 20, free: 98 << 20, avail: 98 << 20},
		"/mnt/my data": {total: 100 << 30, free: 100 << 30, avail: 100 << 30, files: 10, filesFree: 10},
		"/mnt/data":    {total: 4000 << 30, free: 2000 << 30, avail: 2000 << 30},
	}
	c := &Collector{
		root:  root,
		linux: true,
		statfs: func(p string) (fsStats, error) {
			rel := "/" + strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
			st, ok := disks[rel]
			if !ok {
				t.Fatalf("statfs called for unexpected path %q", rel)
			}
			return st, nil
		},
		now:    func() time.Time { return now },
		numCPU: func() int { return 2 },
	}
	return c, root, &now
}

func TestHost(t *testing.T) {
	c, _, _ := newTestCollector(t)
	h := c.Host()
	if h.Hostname != "web-1" || h.OSName != "Ubuntu" || h.OSVersion != "24.04" || h.OSPretty != "Ubuntu 24.04.1 LTS" {
		t.Errorf("os/hostname: %+v", h)
	}
	if h.Kernel != "6.8.0-45-generic" || h.CPUModel != "AMD EPYC 7763 64-Core Processor" || h.CPUCores != 2 {
		t.Errorf("kernel/cpu: %+v", h)
	}
	if h.MemoryTotal != 2013804*1024 || h.BootTime != 1794100000 {
		t.Errorf("memory/boot: %+v", h)
	}
	if len(h.MachineID) != 32 || strings.Contains(h.MachineID, "0123456789abcdef0123") {
		t.Errorf("machine id must be a 32-char hash, not the raw id: %q", h.MachineID)
	}
}

func TestSampleFirstReadingHasNoRates(t *testing.T) {
	c, _, _ := newTestCollector(t)
	s, errs := c.Sample()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if s.CPU != nil || s.DiskIO != nil {
		t.Errorf("rates must be omitted on the first reading: cpu=%v io=%v", s.CPU, s.DiskIO)
	}
	if s.Net == nil || s.Net.RxTotal != 100000000 || s.Net.TxTotal != 20000000 || len(s.Net.Ifaces) != 0 {
		t.Errorf("first net reading should only carry totals of eth0: %+v", s.Net)
	}
	if s.Uptime != 100010 || s.Procs != 2 || s.TS != 1794200000 {
		t.Errorf("uptime/procs/ts: %+v", s)
	}
	if s.Load == nil || s.Load.L1 != 0.12 || s.Load.L15 != 0.05 {
		t.Errorf("load: %+v", s.Load)
	}
	m := s.Mem
	if m == nil || m.Total != 2013804*1024 || m.Available != 1258240*1024 || m.Used != (2013804-1258240)*1024 || m.SwapUsed != 100000*1024 {
		t.Errorf("mem: %+v", m)
	}
	var mountsSeen []string
	for _, d := range s.Disks {
		mountsSeen = append(mountsSeen, d.Mount+":"+d.FSType)
	}
	if got, want := strings.Join(mountsSeen, ","), "/:ext4,/boot/efi:vfat,/mnt/data:fuseblk,/mnt/my data:xfs"; got != want {
		t.Errorf("disks = %s, want %s", got, want)
	}
	if root := s.Disks[0]; root.Used != 12<<30 || root.Avail != 26<<30 || root.InodesUsed != 183500 {
		t.Errorf("root disk: %+v", root)
	}
}

func TestSampleRates(t *testing.T) {
	c, root, now := newTestCollector(t)
	c.Sample()
	writeFiles(t, root, map[string]string{
		"proc/stat":      stat2,
		"proc/net/dev":   netdev2,
		"proc/diskstats": diskstats2,
	})
	*now = now.Add(10 * time.Second)
	s, errs := c.Sample()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	cpu := s.CPU
	if cpu == nil || cpu.Usage != 9 || cpu.User != 6 || cpu.System != 2.5 || cpu.IOWait != 1 || cpu.Steal != 0.5 {
		t.Errorf("cpu: %+v", cpu)
	}
	if s.DiskIO == nil || s.DiskIO.ReadBps != 0 || s.DiskIO.WriteBps != 40960 {
		t.Errorf("disk io (only vda counts): %+v", s.DiskIO)
	}
	n := s.Net
	if n == nil || n.RxBps != 2048 || n.TxBps != 1024 || len(n.Ifaces) != 2 || n.Ifaces[0].Name != "eth0" || n.Ifaces[0].Tunnel {
		t.Errorf("net (lo/docker/veth excluded, tunnel not in totals): %+v", n)
	}
	if tun := n.Ifaces[1]; tun.Name != "tailscale0" || !tun.Tunnel || tun.RxBps != 1000 || tun.TxBps != 500 {
		t.Errorf("tailscale0 should be listed as a tunnel with its own rates: %+v", tun)
	}
	if n.RxTotal != 100020480 || n.TxTotal != 20010240 {
		t.Errorf("totals must exclude the tunnel: rx=%d tx=%d", n.RxTotal, n.TxTotal)
	}
}

func TestCounterResetIsNotASpike(t *testing.T) {
	c, root, now := newTestCollector(t)
	c.Sample()
	// After a reboot every counter starts again from a small number.
	writeFiles(t, root, map[string]string{
		"proc/net/dev":   "  eth0: 10  1 0 0 0 0 0 0 20 1 0 0 0 0 0 0\n",
		"proc/diskstats": " 252 0 vda 1 0 8 0 1 0 8 0 0 0 0 0 0 0 0 0 0\n",
	})
	*now = now.Add(10 * time.Second)
	s, _ := c.Sample()
	if s.Net.RxBps != 0 || s.Net.TxBps != 0 || s.DiskIO.WriteBps != 0 {
		t.Errorf("reset counters should give zero rates: net=%+v io=%+v", s.Net, s.DiskIO)
	}
}

func TestMissingFilesAreReportedNotFatal(t *testing.T) {
	c, root, _ := newTestCollector(t)
	os.Remove(filepath.Join(root, "proc/meminfo"))
	s, errs := c.Sample()
	if s.Mem != nil {
		t.Errorf("mem should be omitted when /proc/meminfo is missing")
	}
	if len(errs) != 1 || !os.IsNotExist(errs[0]) {
		t.Errorf("want one not-exist error, got %v", errs)
	}
	if s.Load == nil || len(s.Disks) == 0 {
		t.Errorf("other sections should still be collected")
	}
}

func TestNonLinux(t *testing.T) {
	c := &Collector{root: "/", linux: false, now: time.Now, numCPU: func() int { return 8 }}
	s, errs := c.Sample()
	if len(errs) != 1 || errs[0] != ErrUnsupported || s.TS == 0 || s.Mem != nil {
		t.Errorf("non-linux sample: %+v %v", s, errs)
	}
	if h := c.Host(); h.CPUCores != 8 || h.Hostname == "" {
		t.Errorf("non-linux host: %+v", h)
	}
}

func TestHungFilesystemIsSkippedNotStacked(t *testing.T) {
	c, root, _ := newTestCollector(t)
	release := make(chan struct{})
	defer close(release)
	var hungCalls int
	var mu sync.Mutex
	inner := c.statfs
	c.statfsTimeout = 50 * time.Millisecond
	c.statfs = func(p string) (fsStats, error) {
		if strings.HasSuffix(p, "/mnt/data") {
			mu.Lock()
			hungCalls++
			mu.Unlock()
			<-release // a FUSE daemon that never answers
		}
		return inner(p)
	}
	_ = root

	s, errs := c.Sample()
	if hasMount(s.Disks, "/mnt/data") || !hasMount(s.Disks, "/") {
		t.Errorf("hung mount should be skipped, others kept: %+v", s.Disks)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "no answer within") {
		t.Errorf("want one timeout error, got %v", errs)
	}

	s, errs = c.Sample()
	if hasMount(s.Disks, "/mnt/data") || len(errs) != 1 || !strings.Contains(errs[0].Error(), "still not answering") {
		t.Errorf("second reading: disks=%+v errs=%v", s.Disks, errs)
	}
	mu.Lock()
	calls := hungCalls
	mu.Unlock()
	if calls != 1 {
		t.Errorf("statfs on the hung mount was called %d times, want 1 (no pile-up)", calls)
	}
}

func TestRecoveredFilesystemComesBack(t *testing.T) {
	c, _, _ := newTestCollector(t)
	release := make(chan struct{})
	inner := c.statfs
	c.statfsTimeout = 50 * time.Millisecond
	c.statfs = func(p string) (fsStats, error) {
		if strings.HasSuffix(p, "/mnt/data") {
			<-release
		}
		return inner(p)
	}
	c.Sample() // times out
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, errs := c.Sample()
		if hasMount(s.Disks, "/mnt/data") && len(errs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mount never came back: %+v %v", s.Disks, errs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasMount(ds []protocol.Disk, m string) bool {
	for _, d := range ds {
		if d.Mount == m {
			return true
		}
	}
	return false
}
