# Pebril

**Early development. This prerelease is not ready for production use.**

[Website](https://pebril.dev/) · [Releases](https://github.com/kserksi/Pebril/releases)

## Current version

The development baseline is **0.0.1-dev**. It provides service startup, validated configuration and hot reload, SQLite initialization, native capability checks and library installation, and a basic status page. Image uploading is not available in this build. Interfaces and configuration may change.

## Run

Download the Linux x64 archive and SHA256SUMS from Releases. Verify the archive and extract it into an empty directory:

```bash
sha256sum -c SHA256SUMS
tar -xzf pebril-0.0.1-dev-linux-amd64.tar.gz
cd pebril-0.0.1-dev-linux-amd64
./pebril version
./pebril check
./pebril
```

The default address is `127.0.0.1:8080`. `/` displays service status, `/healthz` reports readiness, and `/api/v1/status` returns status data.

Paths are resolved relative to the executable. Missing configuration generates `config.json`; application data uses `data/`, and native libraries use `bin/`. Copy `.env.example` to `.env` to customize deployment settings. Deployment changes require a restart. Valid runtime JSON changes apply automatically; invalid changes retain the previous configuration.

`check` validates configuration and native capabilities without opening the application database. It may restore or download missing native libraries. Initial setup requires HTTPS access, trusted CA certificates, and a writable native-library directory. A failed core check prevents service startup.

The supplied binary requires Linux x64, glibc 2.34 or newer, and the system C/C++ runtimes. The automatically installed runtime requires SSSE3, SSE4.1, SSE4.2, and POPCNT CPU support. Valid installed libraries can be reused offline.

## Build and checks

With Go, GCC, Rust/Cargo, pkg-config, Python, and ELF inspection tools available, run `./scripts/build.sh`. Build output is placed in `dist/`. Dependency versions are fixed in go.mod/go.sum and native/Cargo.lock. Deployment does not require build tools.

Applicable checks include Go build/vet/test, Rust tests, script syntax checks, and `python3 scripts/test_integration.py`. Optional compatibility checks are provided by the scripts directory. These checks cover the foundation; full product acceptance is incomplete.

## Feedback and licensing

Report reproducible problems through repository issues. Do not include credentials, actual configuration, databases, or user data. Report security problems privately when private vulnerability reporting is available.

Development versions use `x.y.z-dev`; the first stable release will be `1.0.0`. Development releases are marked as prereleases.

Pebril is licensed under [MIT](LICENSE). Third-party components retain their own licenses, and required notices accompany distributed code.
