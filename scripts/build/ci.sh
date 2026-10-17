#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"
case "$root" in /mnt/*) echo 'Build on the native Linux filesystem' >&2; exit 1;; esac
export GOCACHE="$root/.build/go-cache" GOMODCACHE="$root/.build/go-mod" GOPATH="$root/.build/go-path"
export CARGO_HOME="$root/.build/cargo" CARGO_TARGET_DIR="$root/.build/rust-target"
export TMPDIR="$root/.build/tmp" GOTMPDIR="$root/.build/tmp"
export CGO_ENABLED=1 GOFLAGS=-mod=readonly
export PEBRIL_BUILD_OUTPUT="$root/.build/ci/output" PEBRIL_TEST_OUTPUT="$root/.build/ci/output"
export PEBRIL_TEST_RUNTIME="$root/.build/ci/native/lib/libvips.so.42"
export PKG_CONFIG_PATH="$root/.build/ci/native/lib/pkgconfig"
export LD_LIBRARY_PATH="$root/.build/ci/native/lib"
mkdir -p "$TMPDIR"
go version
rustc --version
python3 --version
node --version
python3 scripts/build/ci.py prepare
bash scripts/build.sh
go build -tags nomsgpack,image_worker ./...
go vet -tags nomsgpack,image_worker ./...
go test -tags nomsgpack,image_worker -race -count=1 ./...
cargo test --locked --manifest-path native/Cargo.toml
bash -n scripts/build.sh scripts/pebril.sh scripts/build/ci.sh
node --check internal/server/web/app.js
node tests/web/test_uploads.mjs
node tests/web/test_accounts.mjs
python3 tests/packaging/test_ci.py
python3 tests/integration/test_bootstrap.py
python3 tests/integration/test_server.py
python3 tests/integration/test_accounts.py
python3 tests/integration/test_admin_reset.py
python3 tests/integration/test_uploads.py
python3 tests/integration/test_conversion.py
python3 tests/integration/test_resources.py
rust_notices="$(rustc --print sysroot)/share/doc/rust"
mkdir -p "$PEBRIL_BUILD_OUTPUT/third-party/rust-standard-library"
cp "$rust_notices/COPYRIGHT-library.html" "$PEBRIL_BUILD_OUTPUT/third-party/rust-standard-library/"
cp -R "$rust_notices/licenses" "$PEBRIL_BUILD_OUTPUT/third-party/rust-standard-library/"
python3 scripts/build/ci.py package
