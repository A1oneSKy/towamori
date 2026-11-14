// Command towamori-sign makes and checks the signatures that let a running
// agent trust a new build. Only the release machine has the private key.
//
//	towamori-sign keygen  -out ~/.towamori/agent-release.key     # once; prints the public key
//	towamori-sign sign    -key KEY -version 0.3.0 -os linux -arch amd64 towamori-agent_linux_amd64.tar.gz
//	towamori-sign verify  -pub BASE64 -version 0.3.0 -os linux -arch amd64 towamori-agent_linux_amd64.tar.gz
//
// sign writes <archive>.sig next to the archive.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/towamori/agent/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "towamori-sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: towamori-sign keygen|sign|verify (see the comment at the top of main.go)")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "where to write the private key (refuses to overwrite)")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	return nil
}

func readKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a towamori-sign private key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func statement(archive, version, goos, goarch string) (update.Statement, error) {
	raw, err := os.ReadFile(archive)
	if err != nil {
		return update.Statement{}, err
	}
	sum := sha256.Sum256(raw)
	f, err := os.Open(archive)
	if err != nil {
		return update.Statement{}, err
	}
	defer f.Close()
	bin, err := update.BinarySHA(f)
	if err != nil {
		return update.Statement{}, err
	}
	return update.Statement{Version: version, OS: goos, Arch: goarch, ArchiveSHA: hex.EncodeToString(sum[:]), BinarySHA: bin}, nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	key := fs.String("key", "", "private key file from keygen")
	version := fs.String("version", "", "release number, such as 0.3.0")
	goos := fs.String("os", "linux", "operating system the archive is for")
	goarch := fs.String("arch", "", "CPU the archive is for: amd64 or arm64")
	fs.Parse(args)
	if fs.NArg() != 1 || *key == "" || *version == "" || *goarch == "" {
		return fmt.Errorf("usage: sign -key KEY -version V -os OS -arch ARCH archive.tar.gz")
	}
	priv, err := readKey(*key)
	if err != nil {
		return err
	}
	st, err := statement(fs.Arg(0), *version, *goos, *goarch)
	if err != nil {
		return err
	}
	out, err := update.Sign(priv, st)
	if err != nil {
		return err
	}
	return os.WriteFile(fs.Arg(0)+".sig", out, 0o644)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	pub := fs.String("pub", update.KeyB64, "public key, base64 (default: the one built into this tool)")
	version := fs.String("version", "", "expected release number")
	goos := fs.String("os", "linux", "expected operating system")
	goarch := fs.String("arch", "", "expected CPU")
	fs.Parse(args)
	if fs.NArg() != 1 || *goarch == "" || *version == "" {
		return fmt.Errorf("usage: verify [-pub KEY] -version V -os OS -arch ARCH archive.tar.gz")
	}
	raw, err := base64.StdEncoding.DecodeString(*pub)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("bad public key")
	}
	sig, err := os.ReadFile(fs.Arg(0) + ".sig")
	if err != nil {
		return err
	}
	got, err := update.Parse(ed25519.PublicKey(raw), sig)
	if err != nil {
		return err
	}
	want, err := statement(fs.Arg(0), *version, *goos, *goarch)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("the signature is valid but is about something else:\n  signed: %+v\n  this:   %+v", got, want)
	}
	fmt.Println("ok:", got.Version, got.OS+"/"+got.Arch)
	return nil
}
