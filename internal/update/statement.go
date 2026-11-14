// Package update lets the agent replace itself with a newer build, but only a
// build that Towamori signed, and only when the platform has been asked (by
// you, in the console) to offer one.
//
// What makes this safe to leave switched on:
//
//   - Every release archive comes with a small signed statement (a ".sig" file)
//     that says which version, OS and CPU it is for, and the SHA-256 of the
//     archive and of the program inside it. The agent has the matching public
//     key built in; a download that does not verify is thrown away.
//   - The signing key never leaves the machine that builds releases. A
//     compromised platform or download server can refuse to update you, but
//     cannot make an agent run anything that was not built and signed by us.
//   - Nothing happens until you press "Update" in the console (the platform
//     answers a report with an offer only then), and TOWAMORI_UPDATE=off
//     switches the whole thing off on a server.
//   - The new build is started in place of the old one; if it fails to report
//     in three tries the agent goes back to the build the package manager
//     (or install.sh) put there and says why.
package update

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// KeyB64 is the release public key (Ed25519, standard base64). Release builds
// use this default; tests replace it with -ldflags "-X …update.KeyB64=…".
var KeyB64 = "yuD6hAI5ekfataPXIXl5SxYZD+QQbvEOtYKIsJG2hkI="

// Header is the first line of every statement, so a signature made for some
// other purpose can never be mistaken for one.
const Header = "towamori-agent-update v1"

// Statement is what a release signature vouches for.
type Statement struct {
	Version    string // 0.3.0
	OS         string // linux
	Arch       string // amd64
	ArchiveSHA string // hex SHA-256 of the .tar.gz
	BinarySHA  string // hex SHA-256 of the towamori-agent file inside it
}

var (
	versionRe = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
	hexRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	wordRe    = regexp.MustCompile(`^[a-z0-9]{2,12}$`)
)

// ValidVersion reports whether s is a plain release number such as 0.3.0.
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

func (s Statement) valid() error {
	switch {
	case !ValidVersion(s.Version):
		return fmt.Errorf("bad version %q", s.Version)
	case !wordRe.MatchString(s.OS) || !wordRe.MatchString(s.Arch):
		return errors.New("bad os or arch")
	case !hexRe.MatchString(s.ArchiveSHA) || !hexRe.MatchString(s.BinarySHA):
		return errors.New("bad checksum")
	}
	return nil
}

// text is the part of the file that is signed.
func (s Statement) text() []byte {
	return []byte(fmt.Sprintf("%s\nversion=%s\nos=%s\narch=%s\narchive_sha256=%s\nbinary_sha256=%s\n",
		Header, s.Version, s.OS, s.Arch, s.ArchiveSHA, s.BinarySHA))
}

// Sign returns the contents of a .sig file for s.
func Sign(priv ed25519.PrivateKey, s Statement) ([]byte, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	body := s.text()
	sig := ed25519.Sign(priv, body)
	return append(body, []byte("signature="+base64.StdEncoding.EncodeToString(sig)+"\n")...), nil
}

// Parse reads a .sig file and checks its signature against pub. It does not
// check that the statement is about the thing you downloaded; the caller does.
func Parse(pub ed25519.PublicKey, file []byte) (Statement, error) {
	if len(file) > 4096 {
		return Statement{}, errors.New("signature file too large")
	}
	i := bytes.LastIndex(file, []byte("signature="))
	if i <= 0 || (i > 0 && file[i-1] != '\n') {
		return Statement{}, errors.New("no signature in file")
	}
	body, tail := file[:i], strings.TrimSpace(string(file[i+len("signature="):]))
	sig, err := base64.StdEncoding.DecodeString(tail)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Statement{}, errors.New("malformed signature")
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, body, sig) {
		return Statement{}, errors.New("signature does not match")
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if line != Header {
				return Statement{}, errors.New("not a Towamori agent update statement")
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Statement{}, errors.New("malformed statement")
		}
		kv[k] = v
	}
	st := Statement{Version: kv["version"], OS: kv["os"], Arch: kv["arch"], ArchiveSHA: kv["archive_sha256"], BinarySHA: kv["binary_sha256"]}
	if err := st.valid(); err != nil {
		return Statement{}, err
	}
	if !bytes.Equal(st.text(), body) {
		return Statement{}, errors.New("statement has extra or reordered fields")
	}
	return st, nil
}

// PublicKey decodes KeyB64.
func PublicKey() (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(KeyB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("this build has no release key")
	}
	return ed25519.PublicKey(raw), nil
}

// Newer reports whether version a is a later release than b. Anything that is
// not a plain release number (a development build) is never newer or older.
func Newer(a, b string) bool {
	pa, ok1 := parse(a)
	pb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	if !ValidVersion(v) {
		return out, false
	}
	fmt.Sscanf(v, "%d.%d.%d", &out[0], &out[1], &out[2])
	return out, true
}
