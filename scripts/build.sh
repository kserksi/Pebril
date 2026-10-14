#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
case "$root" in /mnt/*) echo 'Build on the native WSL filesystem' >&2; exit 1;; esac
export GOCACHE="$root/.build/go-cache" GOMODCACHE="$root/.build/go-mod" GOPATH="$root/.build/go-path"
export CARGO_HOME="$root/.build/cargo" CARGO_TARGET_DIR="$root/.build/rust-target"
mkdir -p dist/bin
cargo build --locked --release --manifest-path native/Cargo.toml
cp "$CARGO_TARGET_DIR/release/libpebril_native.so" dist/bin/
python3 - <<'PYCODE'
import gzip,os
from pathlib import Path
Path("internal/nativeassets/runtime_native.gz").write_bytes(gzip.compress((Path(os.environ["CARGO_TARGET_DIR"])/"release/libpebril_native.so").read_bytes(),mtime=0))
PYCODE
trap 'rm -f internal/nativeassets/runtime_native.gz' EXIT
CGO_ENABLED=1 go build -tags nomsgpack,bundled_native -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/pebril ./cmd/pebril
cp configs/.env.example configs/config.example.json LICENSE dist/
python3 scripts/build/collect_licenses.py
printf 'Built %s; a missing compatible libvips triggers pinned-runtime installation and self-tests.\n' "$(cat VERSION)"

python3 - <<'PYCODE'
import hashlib,json,re,subprocess
from pathlib import Path
for binary in [Path("dist/pebril"),Path("dist/bin/libpebril_native.so")]:
    versions=re.findall(r"GLIBC_(\d+)\.(\d+)",subprocess.check_output(["readelf","--version-info",str(binary)]).decode())
    maximum=max((int(a),int(b)) for a,b in versions)
    if maximum>(2,34):raise SystemExit(f"{binary} requires glibc {maximum}, exceeding the minimum target 2.34")
files=[Path("dist/pebril"),Path("dist/bin/libpebril_native.so"),Path("dist/.env.example"),Path("dist/config.example.json"),Path("dist/LICENSE")]
files.extend(p for p in Path("dist/third-party").rglob("*") if p.is_file())
Path("dist/checksums.json").write_text(json.dumps({str(p.relative_to("dist")):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(files)},indent=2)+"\n")
PYCODE
