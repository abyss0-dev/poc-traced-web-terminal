#!/usr/bin/env bash
# Downloads one Ubuntu cloud image as a read-only base, derives three
# copy-on-write overlays from it, and builds a cloud-init seed for each target.
# Re-running is idempotent: existing artifacts are left in place.
set -euo pipefail

VM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLOUD_INIT_DIR="${VM_DIR}/cloud-init"

IMAGE_URL="${IMAGE_URL:-https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img}"
BASE_IMG="${VM_DIR}/base/ubuntu-base.img"
OVERLAY_SIZE="${OVERLAY_SIZE:-10G}"
TARGETS=(vm1 vm2 vm3)

# bloodhound static binary, baked into every guest's cloud-init seed so the
# tracer is resident by default at boot. Override BLOODHOUND_BIN to point at a
# different build; the default is the bloodhound repo's static docker output.
BLOODHOUND_BIN="${BLOODHOUND_BIN:-${VM_DIR}/../../../bloodhound/target/docker/bloodhound}"
BLOODHOUND_PLACEHOLDER='@@BLOODHOUND_GZ_B64@@'

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: missing required tool: $1" >&2; exit 1; }; }

need qemu-img
need curl
need gzip
need base64

if [[ ! -f "${BLOODHOUND_BIN}" ]]; then
  echo "error: bloodhound binary not found: ${BLOODHOUND_BIN}" >&2
  echo "       build it in the bloodhound repo first, e.g.:" >&2
  echo "         (cd bloodhound && make build-docker)   # -> target/docker/bloodhound" >&2
  echo "       or set BLOODHOUND_BIN=/path/to/bloodhound" >&2
  exit 1
fi

# 1. Base image (downloaded once, then treated as read-only).
mkdir -p "${VM_DIR}/base"
if [[ ! -f "${BASE_IMG}" ]]; then
  echo "downloading base image: ${IMAGE_URL}"
  curl -fSL "${IMAGE_URL}" -o "${BASE_IMG}.tmp"
  mv "${BASE_IMG}.tmp" "${BASE_IMG}"
else
  echo "base image present: ${BASE_IMG}"
fi

# render_user_data writes a copy of the target's user-data with the bloodhound
# binary spliced into the placeholder line as indented gzip+base64. The block
# scalar sits at six-space indentation in the cloud-config, so each base64 line
# is prefixed to match.
render_user_data() {
  local src_ud="$1" out_ud="$2"
  local blob
  blob="$(mktemp)"
  gzip -c "${BLOODHOUND_BIN}" | base64 -w 76 | sed 's/^/      /' >"${blob}"
  awk -v token="${BLOODHOUND_PLACEHOLDER}" -v blobfile="${blob}" '
    index($0, token) { while ((getline line < blobfile) > 0) print line; next }
    { print }
  ' "${src_ud}" >"${out_ud}"
  rm -f "${blob}"
}

# 2. Copy-on-write overlay + cloud-init seed per target.
build_seed() {
  local vm="$1" seed="$2"
  local src="${CLOUD_INIT_DIR}/${vm}"
  # Render into a temp dir under the canonical name so the genisoimage family
  # (which derives the on-ISO filename from the basename) still produces a NoCloud
  # `user-data`; cloud-localds is filename-agnostic but uses the same path.
  local tmp ud
  tmp="$(mktemp -d)"
  ud="${tmp}/user-data"
  render_user_data "${src}/user-data" "${ud}"
  if command -v cloud-localds >/dev/null 2>&1; then
    cloud-localds "${seed}" "${ud}" "${src}/meta-data"
  elif command -v genisoimage >/dev/null 2>&1; then
    genisoimage -output "${seed}" -volid cidata -joliet -rock \
      "${ud}" "${src}/meta-data"
  elif command -v mkisofs >/dev/null 2>&1; then
    mkisofs -output "${seed}" -volid cidata -joliet -rock \
      "${ud}" "${src}/meta-data"
  elif command -v xorriso >/dev/null 2>&1; then
    xorriso -as mkisofs -output "${seed}" -volid cidata -joliet -rock \
      "${ud}" "${src}/meta-data"
  else
    rm -rf "${tmp}"
    echo "error: need cloud-localds, genisoimage, mkisofs, or xorriso to build seeds" >&2
    echo "       install one, e.g.:  sudo apt-get install -y cloud-image-utils" >&2
    exit 1
  fi
  rm -rf "${tmp}"
}

for vm in "${TARGETS[@]}"; do
  overlay="${VM_DIR}/overlay-${vm}.qcow2"
  seed="${VM_DIR}/seed-${vm}.img"

  if [[ ! -f "${overlay}" ]]; then
    echo "creating overlay: ${overlay}"
    qemu-img create -f qcow2 -b "${BASE_IMG}" -F qcow2 "${overlay}" "${OVERLAY_SIZE}"
  else
    echo "overlay present: ${overlay}"
  fi

  echo "building seed: ${seed}"
  build_seed "${vm}" "${seed}"
done

echo "done. overlays and seeds are in ${VM_DIR}"
