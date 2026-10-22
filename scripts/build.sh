#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
case "$root" in /mnt/*) echo 'Build on the native WSL filesystem' >&2; exit 1;; esac
export GOCACHE="$root/.build/go-cache" GOMODCACHE="$root/.build/go-mod" GOPATH="$root/.build/go-path"
export CARGO_HOME="$root/.build/cargo" CARGO_TARGET_DIR="$root/.build/rust-target"
export TMPDIR="$root/.build/tmp" GOTMPDIR="$root/.build/tmp"
export RUSTFLAGS="${RUSTFLAGS:-} --remap-path-prefix=$root=."
export PEBRIL_BUILD_OUTPUT="${PEBRIL_BUILD_OUTPUT:-$root/dist}"
for name in GOCACHE GOMODCACHE GOPATH CARGO_HOME CARGO_TARGET_DIR TMPDIR GOTMPDIR PEBRIL_BUILD_OUTPUT; do
    value=$(realpath -m -- "${!name}")
    case "$value" in /mnt/*) echo "$name must remain on the native WSL filesystem" >&2; exit 1;; esac
    printf -v "$name" '%s' "$value"
    export "$name"
done
mkdir -p "$TMPDIR" "$PEBRIL_BUILD_OUTPUT/bin"
cargo build --locked --release --manifest-path native/Cargo.toml
cp "$CARGO_TARGET_DIR/release/libpebril_native.so" "$PEBRIL_BUILD_OUTPUT/bin/"
trap 'rm -f internal/nativeassets/runtime_native.gz internal/nativeassets/runtime_worker.gz' EXIT
python3 - <<'PYCODE'
import gzip,os
from pathlib import Path
Path("internal/nativeassets/runtime_native.gz").write_bytes(gzip.compress((Path(os.environ["CARGO_TARGET_DIR"])/"release/libpebril_native.so").read_bytes(),mtime=0))
PYCODE
CGO_ENABLED=1 go build -tags image_worker -trimpath -buildvcs=false -ldflags "-s -w" -o "$PEBRIL_BUILD_OUTPUT/bin/pebril-image-worker" ./cmd/pebril-image-worker
python3 - <<'PYWORKER'
import gzip,os
from pathlib import Path
Path("internal/nativeassets/runtime_worker.gz").write_bytes(gzip.compress((Path(os.environ["PEBRIL_BUILD_OUTPUT"])/"bin/pebril-image-worker").read_bytes(),mtime=0))
PYWORKER
CGO_ENABLED=1 go build -tags nomsgpack,bundled_native -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o "$PEBRIL_BUILD_OUTPUT/pebril" ./cmd/pebril
CGO_ENABLED=1 go build -tags nomsgpack,bundled_native -trimpath -buildvcs=false -ldflags "-s -w" -o "$PEBRIL_BUILD_OUTPUT/bin/pebril-runtime" ./cmd/pebril-runtime
cp configs/.env.example configs/config.example.json configs/config.s3.example.json LICENSE VERSION "$PEBRIL_BUILD_OUTPUT/"
install -m 0755 scripts/pebril.sh "$PEBRIL_BUILD_OUTPUT/pebril.sh"
python3 scripts/build/collect_licenses.py
python3 - <<'PYCODE'
import hashlib,json,os,re,subprocess
from pathlib import Path
output=Path(os.environ["PEBRIL_BUILD_OUTPUT"])
binaries=[output/"pebril",output/"bin/libpebril_native.so",output/"bin/pebril-image-worker",output/"bin/pebril-runtime"]
for binary in binaries:
    versions=re.findall(r"GLIBC_(\d+)\.(\d+)",subprocess.check_output(["readelf","--version-info",str(binary)]).decode())
    maximum=max(((int(a),int(b)) for a,b in versions),default=(0,0))
    if maximum>(2,34):raise SystemExit(f"{binary} requires glibc {maximum}, exceeding the minimum target 2.34")
identity={"version":Path("VERSION").read_text().strip(),"source_commit":subprocess.check_output(["git","rev-parse","HEAD"],text=True).strip(),"source_dirty":bool(subprocess.check_output(["git","status","--porcelain","--untracked-files=normal"],text=True).strip()),"target":"linux-x64-glibc","bootstrap":"pebril.sh"}
identity["native_build"]={"pkg_config_version":subprocess.check_output(["pkg-config","--modversion","vips"],text=True).strip(),"startup_catalog":"third-party/native/native-manifest.json"}
for binary in [output/"pebril",output/"bin/pebril-image-worker",output/"bin/pebril-runtime"]:
    build_info=subprocess.check_output(["go","version","-m",str(binary)],text=True)
    version=build_info.splitlines()[0].split()[-1]
    match=re.fullmatch(r"go(\d+)\.(\d+)\.(\d+)",version)
    if not match or (tuple(map(int,match.groups()))<(1,26,10) or (tuple(map(int,match.groups()))[:2]==(1,27) and int(match.group(3))<3)):
        raise SystemExit(f"{binary.name} requires a security-patched Go toolchain >=1.26.10; found {version}")
identity["go_toolchain"]=version
(output/"build.json").write_text(json.dumps(identity,indent=2)+"\n")
files=binaries+[output/name for name in ["pebril.sh","VERSION","build.json",".env.example","config.example.json","config.s3.example.json","LICENSE"]]
files.extend(p for p in (output/"third-party").rglob("*") if p.is_file())
(output/"checksums.json").write_text(json.dumps({str(p.relative_to(output)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(files)},indent=2)+"\n")
PYCODE
printf 'Built %s; launch pebril.sh for independent runtime preparation.\n' "$(cat VERSION)"
