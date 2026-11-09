package collect

import (
	"bufio"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The parsers in this file are pure functions over the text of Linux /proc
// files, so they can be tested on any operating system.

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (t cpuTimes) total() uint64 {
	// guest and guest_nice are already included in user and nice.
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

// parseStat reads the aggregate "cpu" line and "btime" from /proc/stat.
func parseStat(data string) (cpu cpuTimes, bootTime int64, err error) {
	foundCPU := false
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "cpu":
			if len(f) < 5 {
				return cpu, 0, errors.New("/proc/stat: short cpu line")
			}
			v := make([]uint64, 8)
			for i := 0; i < 8 && i+1 < len(f); i++ {
				v[i], err = strconv.ParseUint(f[i+1], 10, 64)
				if err != nil {
					return cpu, 0, fmt.Errorf("/proc/stat: %w", err)
				}
			}
			cpu = cpuTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
			foundCPU = true
		case "btime":
			if len(f) >= 2 {
				bootTime, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	if !foundCPU {
		return cpu, 0, errors.New("/proc/stat: no cpu line")
	}
	return cpu, bootTime, nil
}

type loadAvg struct{ l1, l5, l15 float64 }

func parseLoadAvg(data string) (loadAvg, error) {
	f := strings.Fields(data)
	if len(f) < 3 {
		return loadAvg{}, errors.New("/proc/loadavg: too few fields")
	}
	var v [3]float64
	for i := range v {
		x, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return loadAvg{}, fmt.Errorf("/proc/loadavg: %w", err)
		}
		v[i] = x
	}
	return loadAvg{v[0], v[1], v[2]}, nil
}

type memInfo struct {
	total, available, swapTotal, swapFree uint64
}

func parseMeminfo(data string) (memInfo, error) {
	kv := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) >= 3 && f[2] == "kB" {
			n *= 1024
		}
		kv[strings.TrimSuffix(f[0], ":")] = n
	}
	total, ok := kv["MemTotal"]
	if !ok {
		return memInfo{}, errors.New("/proc/meminfo: no MemTotal")
	}
	avail, ok := kv["MemAvailable"]
	if !ok {
		// Kernels before 3.14 have no MemAvailable; this is the usual estimate.
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
	}
	if avail > total {
		avail = total
	}
	return memInfo{
		total:     total,
		available: avail,
		swapTotal: kv["SwapTotal"],
		swapFree:  kv["SwapFree"],
	}, nil
}

type ifaceCounters struct{ rx, tx uint64 }

// skipIface reports whether an interface is loopback or a container/virtual
// bridge whose traffic is already counted on a physical interface.
func skipIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"docker", "veth", "br-", "virbr", "cni", "flannel", "cali", "kube-", "vnet"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isTunnel reports VPN and tunnel interfaces. Their traffic also crosses a
// physical interface (encrypted or encapsulated), so they are listed but not
// added to the totals.
func isTunnel(name string) bool {
	for _, p := range []string{"tun", "wg", "tailscale", "zt", "ipsec", "gre", "sit", "ip6tnl", "vti"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseNetDev returns byte counters for every interface that is not skipped.
func parseNetDev(data string) (map[string]ifaceCounters, error) {
	out := map[string]ifaceCounters{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue // header lines
		}
		name := strings.TrimSpace(line[:colon])
		f := strings.Fields(line[colon+1:])
		if len(f) < 9 || skipIface(name) {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("/proc/net/dev: bad counters for %s", name)
		}
		out[name] = ifaceCounters{rx, tx}
	}
	return out, nil
}

var (
	wholeDiskSCSI = regexp.MustCompile(`^(sd|vd|xvd|hd)[a-z]+$`)
	wholeDiskNVMe = regexp.MustCompile(`^nvme[0-9]+n[0-9]+$`)
	wholeDiskMMC  = regexp.MustCompile(`^mmcblk[0-9]+$`)
)

// isWholeDisk keeps physical disks and drops partitions and stacked devices
// (loop, ram, zram, device-mapper, md RAID) so that I/O is not counted twice.
func isWholeDisk(name string) bool {
	return wholeDiskSCSI.MatchString(name) || wholeDiskNVMe.MatchString(name) || wholeDiskMMC.MatchString(name)
}

// parseDiskstats returns bytes read and written, summed over whole disks.
func parseDiskstats(data string) (readBytes, writeBytes uint64, err error) {
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// major minor name reads merged sectors_read ms writes merged sectors_written ...
		if len(f) < 10 || !isWholeDisk(f[2]) {
			continue
		}
		r, err1 := strconv.ParseUint(f[5], 10, 64)
		w, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0, fmt.Errorf("/proc/diskstats: bad counters for %s", f[2])
		}
		// The kernel always counts 512-byte sectors here, whatever the device's
		// real sector size.
		readBytes += r * 512
		writeBytes += w * 512
	}
	return readBytes, writeBytes, nil
}

type mount struct {
	device, point, fstype string
}

var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "bcachefs": true, "jfs": true, "reiserfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	// FUSE on a block device: ntfs-3g and exfat-fuse data disks.
	"fuseblk": true,
}

// parseMounts returns real block-device filesystems from /proc/mounts, one
// per device. When a device is mounted more than once (bind mounts, btrfs
// subvolumes) the shortest mount point wins, so "/" beats "/var/lib/docker".
func parseMounts(data string) []mount {
	byDev := map[string]mount{}
	var order []string
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !realFS[f[2]] {
			continue
		}
		m := mount{device: f[0], point: unescapeMount(f[1]), fstype: f[2]}
		prev, seen := byDev[m.device]
		if !seen {
			order = append(order, m.device)
			byDev[m.device] = m
		} else if len(m.point) < len(prev.point) {
			byDev[m.device] = m
		}
	}
	out := make([]mount, 0, len(order))
	for _, d := range order {
		out = append(out, byDev[d])
	}
	return out
}

// unescapeMount decodes the octal escapes (\040 for space, etc.) the kernel
// uses in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseUptime(data string) (int64, error) {
	f := strings.Fields(data)
	if len(f) == 0 {
		return 0, errors.New("/proc/uptime: empty")
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime: %w", err)
	}
	return int64(v), nil
}

type osRelease struct{ name, version, pretty string }

func parseOSRelease(data string) osRelease {
	var r osRelease
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "NAME":
			r.name = v
		case "VERSION_ID":
			r.version = v
		case "PRETTY_NAME":
			r.pretty = v
		}
	}
	return r
}

// parseCPUModel finds a human-readable CPU name in /proc/cpuinfo. x86 uses
// "model name"; some ARM boards use "Model" or "Hardware".
func parseCPUModel(data string) string {
	fallback := ""
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch k {
		case "model name", "cpu model":
			return v
		case "Model", "Hardware":
			if fallback == "" {
				fallback = v
			}
		}
	}
	return fallback
}
