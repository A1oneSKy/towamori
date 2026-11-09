// Package packaging holds install files; this test keeps them consistent.
package packaging

import (
	"os"
	"strings"
	"testing"
)

// install.sh embeds a copy of the systemd unit so it works as a single
// downloaded file. The two copies must not drift apart.
func TestInstallScriptUnitMatchesUnitFile(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile("systemd/towamori-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	start := strings.Index(s, "<<'UNIT_EOF'\n")
	end := strings.Index(s, "\nUNIT_EOF\n")
	if start < 0 || end < start {
		t.Fatal("cannot find the embedded unit in install.sh")
	}
	embedded := s[start+len("<<'UNIT_EOF'\n") : end]
	if got, want := directives(embedded), directives(string(unit)); got != want {
		t.Errorf("install.sh unit differs from systemd/towamori-agent.service\n--- install.sh\n%s\n--- unit file\n%s", got, want)
	}
}

// directives drops comments and blank lines.
func directives(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
