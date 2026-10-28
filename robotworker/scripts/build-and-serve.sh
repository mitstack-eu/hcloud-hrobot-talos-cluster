#!/usr/bin/env bash
# Builds a Robot worker's Talos metal image on the image server with imager,
# decompresses it to image.raw, then runs the serve command it is given.
# Usage: build-and-serve.sh <imager image by digest> <machine configuration> <network router> <serve command>...
#   SERVE_OUT_DIR, SERVE_PULL_ATTEMPTS, SERVE_RESOLV_CONF, SERVE_NET_CLASS and
#   SERVE_ADDRESS_WAIT default to /var/lib/mitstack-image, 30,
#   /etc/resolv.conf, /sys/class/net and 120 seconds.
# It exists because the image embeds every key of the cluster, so it is built
# inside the cluster's own Hetzner project, imager's output, which prints that
# configuration, is discarded, and only the worker may fetch it.
set -euo pipefail

readonly out=${SERVE_OUT_DIR:-/var/lib/mitstack-image}
readonly attempts=${SERVE_PULL_ATTEMPTS:-30}
readonly resolv_conf=${SERVE_RESOLV_CONF:-/etc/resolv.conf}
readonly net_class=${SERVE_NET_CLASS:-/sys/class/net}
readonly address_wait=${SERVE_ADDRESS_WAIT:-120}
readonly resolvers=("185.12.64.1" "185.12.64.2")

fail() {
  echo "build-and-serve: $*" >&2
  exit 1
}

# route_through_gateway waits for the private interface's DHCP lease, then sends the server's traffic to the network's router.
route_through_gateway() {
  local router=$1 links=("$net_class"/en*) waited
  if [ "${#links[@]}" -ne 1 ] || [ ! -e "${links[0]}" ]; then
    fail "found no single ethernet interface in $net_class"
  fi
  for ((waited = 0; waited < address_wait; waited++)); do
    ip -4 -o route get "$router" 2> /dev/null | grep -q " dev ${links[0]##*/} " && break
    sleep 1
  done
  [ "$waited" -lt "$address_wait" ] || fail "the private interface has no route to $router after $address_wait seconds"
  ip route replace default via "$router"
  rm -f "$resolv_conf"
  printf 'nameserver %s\n' "${resolvers[@]}" > "$resolv_conf"
}

pull_imager() {
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    docker pull --quiet "$1" > /dev/null && return 0
    sleep 10
  done
  fail "could not pull $1 after $attempts attempts"
}

# build runs imager with the kit's arguments for the metal profile and discards its output, since it prints every key.
build() {
  local imager=$1 config=$2
  mkdir -p "$out"
  if ! docker run --rm --privileged -v /dev:/dev -v "$config:/in/config.yaml:ro" -v "$out:/out" \
    "$imager" metal --arch amd64 --embedded-config-path /in/config.yaml > /dev/null 2>&1; then
    rm -f "$config"
    fail "imager failed; its output is discarded because it holds every cluster key"
  fi
  rm -f "$config"
  [ -f "$out/metal-amd64.raw.zst" ] || fail "imager left no metal-amd64.raw.zst in $out"
  zstd -d --rm --quiet "$out/metal-amd64.raw.zst" -o "$out/image.raw"
}

main() {
  [ "$#" -ge 4 ] || fail "usage: build-and-serve.sh <imager> <configuration> <router> <serve command>..."
  route_through_gateway "$3"
  pull_imager "$1"
  build "$1" "$2"
  exec "${@:4}"
}

main "$@"
