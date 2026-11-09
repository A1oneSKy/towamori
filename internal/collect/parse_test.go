package collect

import "testing"

func TestIsWholeDisk(t *testing.T) {
	for name, want := range map[string]bool{
		"sda": true, "sdab": true, "vda": true, "xvdf": true, "nvme0n1": true, "mmcblk0": true,
		"sda1": false, "vda15": false, "nvme0n1p2": false, "mmcblk0p1": false,
		"loop0": false, "ram0": false, "zram0": false, "dm-0": false, "md127": false, "sr0": false,
	} {
		if got := isWholeDisk(name); got != want {
			t.Errorf("isWholeDisk(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSkipIface(t *testing.T) {
	for name, want := range map[string]bool{
		"lo": true, "docker0": true, "veth9f2": true, "br-1a2b": true, "virbr0": true, "cni0": true,
		"flannel.1": true, "cali123": true, "kube-ipvs0": true, "vnet3": true,
		"eth0": false, "ens3": false, "enp1s0": false, "wg0": false, "bond0": false,
	} {
		if got := skipIface(name); got != want {
			t.Errorf("skipIface(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMeminfoWithoutMemAvailable(t *testing.T) {
	// Kernels before 3.14.
	m, err := parseMeminfo("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.available != 400*1024 {
		t.Errorf("available = %d, want %d", m.available, 400*1024)
	}
	if _, err := parseMeminfo("garbage"); err == nil {
		t.Error("want error without MemTotal")
	}
}

func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{
		`/mnt/my\040data`: "/mnt/my data",
		`/a\011b`:         "/a\tb",
		`/plain`:          "/plain",
		`/trailing\04`:    `/trailing\04`,
	} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMountsPrefersShortestMountPoint(t *testing.T) {
	ms := parseMounts("/dev/sda2 /var/lib/docker btrfs rw 0 0\n/dev/sda2 / btrfs rw 0 0\n")
	if len(ms) != 1 || ms[0].point != "/" {
		t.Errorf("got %+v", ms)
	}
}

func TestCPUModelFallbacks(t *testing.T) {
	if got := parseCPUModel("processor : 0\nBogoMIPS : 50.00\nModel : Raspberry Pi 4 Model B Rev 1.4\n"); got != "Raspberry Pi 4 Model B Rev 1.4" {
		t.Errorf("arm model = %q", got)
	}
	if got := parseCPUModel("processor : 0\nCPU implementer : 0x41\n"); got != "" {
		t.Errorf("unknown cpu should be empty, got %q", got)
	}
}

func TestParseStatErrors(t *testing.T) {
	if _, _, err := parseStat("intr 1 2 3\n"); err == nil {
		t.Error("want error without cpu line")
	}
	if _, _, err := parseStat("cpu  1 2 x 4 5\n"); err == nil {
		t.Error("want error on non-numeric field")
	}
	// Very old kernels have only four fields; the rest count as zero.
	cpu, _, err := parseStat("cpu  1 2 3 4\n")
	if err != nil || cpu.total() != 10 {
		t.Errorf("short cpu line: %+v %v", cpu, err)
	}
}

func TestIsTunnel(t *testing.T) {
	for name, want := range map[string]bool{
		"tun0": true, "wg0": true, "tailscale0": true, "ztabcdef": true, "gre1": true, "sit0": true,
		"eth0": false, "ens3": false, "enp6s18": false, "bond0": false,
	} {
		if got := isTunnel(name); got != want {
			t.Errorf("isTunnel(%q) = %v, want %v", name, got, want)
		}
	}
}
