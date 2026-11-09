# Contributing

Thanks for helping. A few rules keep the agent small enough to trust.

## Ground rules

1. **No third-party dependencies.** Go standard library only. If you think something needs a
   dependency, open an issue first.
2. **No new powers.** The agent must never accept commands, listen on a port, read files outside the
   ones listed in the README, or send anything not described in [docs/protocol.md](docs/protocol.md).
   Pull requests that add such abilities will be declined, however useful they are.
3. **Protocol changes start in `docs/protocol.md`.** Update the document in the same pull request.
4. **Tests for parsers.** Anything that reads a `/proc` file gets a test with real sample content.

## Developing

```sh
make test
go run ./cmd/towamori-agent --once
```

See the README for running against the local `towamori-devserver`.

## Sign your commits (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/) instead of a CLA.
Add a sign-off line to every commit:

```sh
git commit -s -m "collect: handle missing /proc/diskstats"
```

which adds `Signed-off-by: Your Name <you@example.com>`, meaning you wrote the change or have the
right to submit it under the Apache License 2.0.
