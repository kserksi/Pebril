# Pebril

**Early development. This prerelease is not ready for production use.**

[Website](https://pebril.dev/) · [Releases](https://github.com/kserksi/Pebril/releases)

## What problem does Pebril address?

Pebril is an open-source, self-hosted image hosting project for image-heavy blogs. A post may need many images with different dimensions, formats, and compression quality. Keeping those files organized, preparing suitable images for the web, and obtaining links for an article becomes a repeated workflow.

The project grew from a blogger's search for an actively developed, open-source image host with modern format support. Its intended purpose is to bring image storage, web-oriented conversion, and sharing into one workflow, with processing speed and resource use treated as practical concerns for modest servers.

## Why consider Pebril?

The reasons to follow or contribute to Pebril are its intended priorities:

- **Self-hosting and ownership:** run the service under your control, with source available under MIT.
- **A workflow for image-heavy writing:** manage batches of images and obtain links suitable for articles, including Markdown image syntax.
- **Modern image handling:** prepare web-friendly output while providing clear policies for originals, animation, and supported formats.
- **Resource awareness:** develop and measure the processing pipeline with limited server resources in mind.
- **Visible project maturity:** distinguish implemented capabilities from plans and expose progress through development releases.

These are the project's direction, not a claim that it already outperforms other image hosts. Pebril is currently suitable for evaluating its foundation and participating in development. A working production upload-and-share service is not available yet.

## How do management, conversion, and sharing fit together?

They are three parts of the intended image workflow:

| Part | Responsibility |
|---|---|
| Image management | Keep image records and files organized; manage ownership, storage usage, visibility, and deletion. |
| Image conversion | Validate uploaded content and prepare suitable output formats; handle animation and SVG safety according to the supported rules. |
| Online sharing | Make the selected result accessible through a link, with public or private access appropriate to the image. |

In the planned workflow, an upload creates a managed image, processing prepares its deliverable content, and sharing exposes that content through the image's access rules. Keeping these parts together is intended to reduce manual file conversion, repeated uploading, and separate link management. This complete workflow is still under development.

## What is available today?

The development branch is **0.0.2-dev**. It implements the foundation, not the complete image host. Downloadable builds are listed separately in [Releases](https://github.com/kserksi/Pebril/releases); a development branch version does not imply a matching release asset exists.

| Capability | Current status |
|---|---|
| Service startup, configuration validation, and runtime JSON hot reload | Available |
| SQLite initialization and single-instance protection | Available; image and account management are not implemented |
| Native runtime checks, verified library installation, and internal-library recovery | Available |
| PNG/JPEG/WebP and optional AVIF encode/decode checks | Available as self-tests, not a user-facing conversion API |
| Rust GIF and SVG checks | Available as self-tests, not a complete upload processing pipeline |
| Basic status page, readiness endpoint, and status API | Available |
| Image uploading, gallery management, user accounts, and storage quotas | Planned; unavailable |
| Conversion queues, retries, and upload progress | Planned; unavailable |
| Public/private image sharing, article links, and PicGo upload integration | Planned; unavailable |

Interfaces and configuration may change. Passing foundation checks does not establish production readiness or full image-processing acceptance.

## Where is the project heading?

The first complete version is intended to provide a usable self-hosted image backend, basic APIs, and a simple upload and management website. The planned core connects multi-user image management, modern-format processing, and public/private sharing. The initial deployment focus is a Linux binary with local storage.

Later development may add a fuller administration interface, more storage and deployment options, stronger account protections, and a more refined frontend. Docker deployment, S3-compatible storage, Cloudflare Workers support, and optional Windows support remain later directions, not current capabilities.

The long-term aim is an open-source image host that can support an image-heavy publishing workflow without requiring users to assemble separate management, conversion, and sharing services. Development will proceed incrementally; there is no fixed completion date or published performance guarantee. Development versions use `x.y.z-dev`, and the first stable release will be `1.0.0`.

## Run

Download the Linux x64 archive and SHA256SUMS from Releases. Verify the archive and extract it into an empty directory:

```bash
sha256sum -c SHA256SUMS
tar -xzf pebril-*-linux-amd64.tar.gz
cd pebril-*-linux-amd64
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

Applicable checks include Go build/vet/test, Rust tests, script syntax checks, and `python3 tests/integration/test_server.py`. Optional compatibility checks use `scripts/fetch_compatibility_images.py` and `tests/compatibility/test_runtime.py`. These checks cover the foundation; full product acceptance is incomplete.

## Repository layout

| Directory | Contents |
|---|---|
| `cmd/pebril/` | Application entry point |
| `internal/` | Private Go implementation and unit tests |
| `native/` | Internal Rust library and lockfile |
| `configs/` | Deployment and runtime configuration examples |
| `scripts/` | Build entry point and test preparation tools |
| `tests/integration/` | Service integration checks |
| `tests/compatibility/` | Runtime compatibility checks and pinned inputs |
| `third-party/` | Dependency provenance and license metadata |

The build copies configuration examples into `dist/` for deployment.

## Feedback and licensing

Report reproducible problems through repository issues. Do not include credentials, actual configuration, databases, or user data. Report security problems privately when private vulnerability reporting is available.

Development releases are marked as prereleases.

Pebril is licensed under [MIT](LICENSE). Third-party components retain their own licenses, and required notices accompany distributed code.
