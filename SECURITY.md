# Security policy

The agent runs on other people's servers, so we treat security reports as our top priority.

## Reporting a vulnerability

Email **security@towamori.com**. Please do not open a public issue.

Include what you found, how to reproduce it, and the agent version (`towamori-agent -version`).
We will acknowledge your report within 3 business days and keep you updated until it is fixed.
We are happy to credit you in the release notes.

## Supported versions

Only the latest release receives security fixes. The install script always installs the latest
release.

## In scope

- Anything that lets the platform, or anyone else, make the agent do more than
  [docs/protocol.md](docs/protocol.md) allows: running commands, reading or writing files, opening
  ports, or sending data that the protocol does not list.
- Ways to make the agent send its token somewhere other than the configured endpoint, or over an
  unencrypted connection.
- Problems in `packaging/install.sh` or the systemd unit that weaken the host.
