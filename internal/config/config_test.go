package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrecedenceFlagsOverEnvOverFile(t *testing.T) {
	file := writeEnv(t, "# comment\nTOWAMORI_TOKEN=\"from-file\"\nTOWAMORI_INTERVAL=30\nexport TOWAMORI_ENDPOINT=https://file.example/\n")

	c, err := Parse([]string{"-env-file", file}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "from-file" || c.Interval != 30*time.Second || c.Endpoint != "https://file.example" {
		t.Errorf("file values: %+v", c)
	}

	c, err = Parse([]string{"-env-file", file}, env(map[string]string{"TOWAMORI_TOKEN": "from-env"}), io.Discard)
	if err != nil || c.Token != "from-env" {
		t.Errorf("env should beat file: %+v %v", c, err)
	}

	c, err = Parse([]string{"-env-file", file, "-token", "from-flag", "-interval", "15s"}, env(map[string]string{"TOWAMORI_TOKEN": "from-env"}), io.Discard)
	if err != nil || c.Token != "from-flag" || c.Interval != 15*time.Second {
		t.Errorf("flag should beat env: %+v %v", c, err)
	}
}

func TestDefaultsAndMissingEnvFile(t *testing.T) {
	c, err := Parse([]string{"-env-file", "/nonexistent/agent.env", "-token", "t"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != DefaultEndpoint || c.Interval != DefaultInterval {
		t.Errorf("defaults: %+v", c)
	}
}

func TestRequiresToken(t *testing.T) {
	_, err := Parse([]string{"-env-file", ""}, env(nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("want no-token error, got %v", err)
	}
	if c, err := Parse([]string{"-env-file", "", "-once"}, env(nil), io.Discard); err != nil || !c.Once {
		t.Errorf("-once must work without a token: %v", err)
	}
}

func TestIntervalBounds(t *testing.T) {
	for _, v := range []string{"1s", "301s", "0"} {
		if _, err := Parse([]string{"-env-file", "", "-token", "t", "-interval", v}, env(nil), io.Discard); err == nil && v != "0" {
			t.Errorf("interval %s should be rejected", v)
		}
	}
}

func TestCheckEndpoint(t *testing.T) {
	ok := []string{"https://ingest.towamori.com", "http://localhost:8787", "http://127.0.0.1:8787", "http://[::1]:8787"}
	bad := []string{"http://ingest.towamori.com", "http://10.0.0.5:8787", "ftp://x", "not a url", "https://"}
	for _, e := range ok {
		if err := CheckEndpoint(e); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
	for _, e := range bad {
		if err := CheckEndpoint(e); err == nil {
			t.Errorf("%s should be rejected", e)
		}
	}
}

func TestBadEnvFileLine(t *testing.T) {
	file := writeEnv(t, "TOWAMORI_TOKEN\n")
	if _, err := Parse([]string{"-env-file", file}, env(nil), io.Discard); err == nil {
		t.Error("want a parse error for a line without =")
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("twa_abcdefghijkl"); got != "twa_ab…" {
		t.Errorf("Redact = %q", got)
	}
	if got := Redact("short"); got != "****" {
		t.Errorf("Redact short = %q", got)
	}
}

// Under systemd the service runs as an unprivileged user and the settings
// file is readable by root only; systemd passes the values in the environment,
// so the agent must start anyway. This is what made the first installs on
// real servers restart in a loop.
func TestUnreadableDefaultEnvFileIsIgnored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	file := writeEnv(t, "TOWAMORI_TOKEN=secret\n")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	old := DefaultEnvFile
	DefaultEnvFile = file
	defer func() { DefaultEnvFile = old }()

	c, err := Parse(nil, env(map[string]string{"TOWAMORI_TOKEN": "from-systemd"}), io.Discard)
	if err != nil || c.Token != "from-systemd" {
		t.Fatalf("an unreadable default file must not stop the agent: %+v %v", c, err)
	}
	// A file somebody named explicitly has to be readable.
	other := writeEnv(t, "TOWAMORI_TOKEN=secret\n")
	if err := os.Chmod(other, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]string{"-env-file", other}, env(map[string]string{"TOWAMORI_TOKEN": "x"}), io.Discard); err == nil {
		t.Error("an unreadable file passed with -env-file should be an error")
	}
}
