# Pebril

<p align="center"><strong><em>Peril — like a pebble in a river, giving your pictures a place to rest.</em></strong></p>

**Early development. This prerelease is not ready for production use.**

[Website](https://pebril.dev/) · [Releases](https://github.com/kserksi/Pebril/releases) · [Roadmap](https://github.com/kserksi/Pebril/issues?q=is%3Aissue%20label%3Aroadmap)

## What problem does Pebril address?

Pebril is an open-source, self-hosted image hosting project for image-heavy blogs. Its intended purpose is to bring image storage, web-oriented conversion, and sharing into one workflow, with practical resource use on modest servers.

## Why consider Pebril?

- **Self-hosting and ownership:** keep images under your control, with source available under MIT.
- **A workflow for writing:** upload images, prepare web-friendly results, and copy URLs or Markdown image links.
- **Modern image handling:** support common static and animated formats, with clear handling of originals and SVG safety.

The complete image host is still under development. Current releases are suitable for evaluation and contribution, with no published performance guarantee.

## How do management, conversion, and sharing fit together?

| Part | Responsibility |
|---|---|
| Image management | Organize records, ownership, files, and storage usage. |
| Image conversion | Validate uploads and prepare suitable output formats. |
| Online sharing | Provide links with the image's access rules. |

Conversion, public result links, and basic gallery management are available. Private sharing and batch gallery management remain planned.

## What is available today?

The `dev` branch is at **0.0.8-dev**, with ordinary account registration/administration and a basic gallery. The latest published prerelease is [0.0.8-dev](https://github.com/kserksi/Pebril/releases/tag/v0.0.8-dev).

| Capability | Current status |
|---|---|
| Configuration, JSON hot reload, SQLite, and readiness checks | Available |
| Administrator initialization, login/logout, and upload-only tokens | Available |
| Image reception, content validation, quota checks, and safe storage | Available |
| Static conversion | PNG, JPEG, WebP, and deployment-enabled AVIF output |
| Animation and SVG | Animated WebP/GIF, WebP covers, and sanitized SVG delivery |
| Processing feedback and sharing | Status polling, retries, public results, and ordered URL/Markdown copying |
| Runtime setup | Independent Bash launcher, consented downloads, offline reuse, and component recovery |
| Ordinary accounts | Registration, account status, and administrator controls for ordinary-user status, quota, and password reset |
| Basic gallery | Paginated owner/administrator views, image details, and individual deletion |
| Original downloads | Authenticated owner/administrator downloads when enabled and an original is retained |
| Email, self-service password recovery, and account deletion/export | Planned; unavailable |
| Batch gallery management | Planned; unavailable |
| Resource controls | Memory-based concurrency, restricted startup, and administrator warnings |
| Private sharing and PicGo integration | Planned; unavailable |

Official system-package installation and recovery validation remain pending.

## Where is the project heading?

The first complete version is intended to connect multi-user image management, modern-format processing, and public/private sharing. Linux binaries with local storage come first. Docker, S3-compatible storage, Cloudflare Workers, and optional Windows support remain later directions. See the Roadmap for progress; unfinished work has no new estimated completion dates.

Development versions use `x.y.z-dev`. The first stable release will be `1.0.0`.

## Run

Download the Linux x64 archive and `SHA256SUMS` from Releases. Verify and extract into an empty directory:

```bash
sha256sum -c SHA256SUMS
tar -xzf pebril-*-linux-amd64-*.tar.gz
cd pebril-*-linux-amd64-*
./pebril.sh version
./pebril.sh --check
./pebril.sh
```

Run as an ordinary user. Root service startup is refused. The launcher reuses compatible installed libraries and asks before downloading missing runtimes. It tries a verified upstream package, then the Pebril-maintained fallback if needed. Failure stops startup.

Noninteractive setup requires an explicit download grant for that invocation:

```bash
./pebril.sh --allow-downloads --check
```

This checks dependencies without starting the service. Consent is not saved. Direct `./pebril check` checks configuration and existing libraries offline; it does not download them.

If the detected program memory ceiling is below 40% of system memory, startup requires consent and limits concurrency to one with AVIF conversion disabled. For noninteractive startup, use `./pebril.sh --allow-low-memory`; download consent is separate. The grant applies only to that invocation. Checks report this condition without starting the service.

The package requires Linux x64, glibc >=2.34, compatible system C/C++ runtimes, Bash, coreutils, and flock. Downloaded runtimes also require SSSE3, SSE4.1, SSE4.2, and POPCNT. Downloads need HTTPS access, trusted CA certificates, and a library directory owned by the service user and not writable by others. Deployment requires no compilers; valid runtimes can be reused offline.

The default address is `127.0.0.1:8080`. `/` and `/login` show the basic account, upload, and gallery page; `/healthz` and `/api/v1/status` provide service status. On first deployment, initialize the administrator. Ordinary users can then register and sign in to upload and manage their images. Administrators can manage ordinary users and inspect their images. No default account or password is supplied.

PNG, JPEG, GIF, WebP, and SVG uploads are supported when enabled. AVIF requires deployment capability and its format switch; APNG is rejected. Raster uploads convert asynchronously to WebP at original dimensions and quality 80 by default. Originals are retained, animation receives a separate cover, and SVG is sanitized without rasterization. Configured resizing preserves aspect ratio without enlargement. Transparent JPEG output uses white.

Upload-only tokens use `POST /api/v1/uploads` with a multipart `file` field and `Authorization: Bearer <token>`. HTTP201 indicates completion; HTTP202 is a processing receipt. Owner sessions can poll `GET /api/v1/uploads/:id`; upload tokens cannot query or retry tasks. This is not yet a PicGo-compatible workflow. The page shows processing failures and eligible temporary links or manual retries.

Deploy external access behind HTTPS. Web sessions use protected cookies; web writes require same-origin requests and CSRF protection. Never put credentials in URLs.

Paths resolve relative to the executable. Copy `.env.example` to `.env` for deployment settings, which require restart. Missing runtime configuration generates `config.json`; valid JSON changes apply automatically, and invalid changes retain the last valid configuration. Data uses `data/`, and native libraries use `bin/`.

For upgrades, stop the service, back up configuration and data, and deploy into a new directory while preserving configured data paths. Do not replace live configuration with examples.

## Resource controls

Memory is sampled every 10 seconds for the program and its processing children. From 80% to below 95% of the detected ceiling, concurrency drops by one per sample. At 95% or above, new uploads and conversion starts pause; accepted transfers and running conversions may finish. Below 40%, processing resumes and concurrency increases by one per sample up to the effective limit. Incomplete samples pause new work until a complete recovery sample.

Shared pages may be counted repeatedly, causing earlier degradation. Filesystems carrying application data and temporary files warn above 80% usage. Warning changes are logged, startup reports active warnings, and administrators see a summary when signing in.

## Build and checks

With Go, GCC, Rust/Cargo, pkg-config, compatible libvips development files, Python, and ELF inspection tools available, run `./scripts/build.sh`. Output is placed in `dist/`; Go and Rust lockfiles pin dependencies. GitHub Actions builds and tests pushes to `dev` and `main` and pull requests targeting them. Checked packages are retained as Actions artifacts for seven days. For new development versions, successful `dev` pushes also publish prereleases; pull requests and `main` do not publish Releases.

For each release, synchronize this README's version, release/download links, available capabilities, and remaining scope before building and pushing. Verify the packaged README matches the approved source, then check the live README and release links after publication. Do not claim an unpublished version is available. Published tags and assets remain unchanged when correcting documentation.

Checks cover Go build/vet/race with `nomsgpack,image_worker` tags, Rust, JavaScript logic, accounts/gallery, uploads/conversion, and bootstrap behavior. Integration scripts are in `tests/integration/`; pinned Linux userspace checks are in `tests/compatibility/`. Bootstrap tests use explicit helper doubles for authorization paths. Debian 12, Ubuntu 22.04, and Rocky Linux 9.8 userspace checks share one kernel; they do not establish independent-machine compatibility. Resource checks cover startup consent, concurrency and cooldown, AVIF task recovery, and administrator-only warnings. Low-memory integration uses controlled memory metadata, not an enforced memory quota. Real privileged installation, representative memory-pressure benchmarks, and full-product acceptance remain open.

## Repository layout

| Directory | Contents |
|---|---|
| `cmd/` | Service, conversion worker, and deployment utility |
| `internal/` | Private Go packages, tests, and basic web page |
| `native/` | Rust library and lockfile |
| `configs/` | Deployment examples |
| `scripts/` | Build, deployment, and test preparation |
| `tests/` | Integration, compatibility, and web checks |
| `third-party/` | Dependency provenance and notices |

## Feedback and licensing

Report reproducible problems through repository issues without credentials, actual configuration, databases, or user data. Report security problems privately when private vulnerability reporting is available.

Pebril is licensed under [MIT](LICENSE). Third-party components retain their own licenses, and required notices accompany distributed code.
