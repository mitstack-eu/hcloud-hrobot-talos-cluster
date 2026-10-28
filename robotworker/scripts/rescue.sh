#!/usr/bin/env bash
# Runs in Hetzner's rescue system on a Robot worker. "read" prints each disk
# with its serial, the MAC of the card the default route leaves by, and
# whether the firmware booted by UEFI. "write" brings up the vSwitch VLAN,
# waits until the install's image server answers over it, clears every disk's
# arrays and signatures, and streams the image onto the disk with the given
# serial.
# Usage: rescue.sh read
#        rescue.sh write <vlan> <address/prefix> <vswitch gateway> <image server> <port> <pin> <serial>
#   RESCUE_SYS, RESCUE_DEV, RESCUE_FETCH_ATTEMPTS and RESCUE_FETCH_INTERVAL
#   default to /sys, /dev, 90 and 10 seconds.
# It exists because Robot names no host key for the rescue system, so the
# session that runs this carries no secret: the image, which holds the
# cluster's keys, reaches the disk only over the vSwitch, from a server whose
# TLS key the rescue checks against the pin.
set -euo pipefail

readonly sys=${RESCUE_SYS:-/sys}
readonly dev=${RESCUE_DEV:-/dev}
readonly attempts=${RESCUE_FETCH_ATTEMPTS:-90}
readonly interval=${RESCUE_FETCH_INTERVAL:-10}
readonly vswitch_link=vsw0
readonly vswitch_mtu=1400
# curl's exit status when the server's key is not the pinned one.
readonly pin_refused=90

fail() {
  echo "rescue: $*" >&2
  exit 1
}

# disks prints the kernel name of each NVMe and SATA disk.
disks() {
  local d
  for d in "$sys"/block/nvme*n* "$sys"/block/sd*; do
    [ -e "$d" ] || continue
    echo "${d##*/}"
  done
}

serial_of() {
  local d=$sys/block/$1
  tr -d ' \n' < "$d/device/serial" 2> /dev/null || tr -d ' \n' < "$d/serial" 2> /dev/null || true
}

public_link() {
  local link
  link=$(ip -o route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}')
  [ -n "$link" ] || fail "found no default route, so no public card"
  echo "$link"
}

read_hardware() {
  local d link mac size
  if [ -d "$sys/firmware/efi" ]; then echo "efi yes"; else echo "efi no"; fi
  link=$(public_link)
  mac=$(cat "$sys/class/net/$link/address")
  echo "mac $mac"
  for d in $(disks); do
    size=$(cat "$sys/block/$d/size")
    echo "disk $d $(serial_of "$d") $((size * 512))"
  done
}

# clear_disks stops every array and volume group and wipes every signature, as the trial did before its write.
clear_disks() {
  local d part md vg
  for vg in $(vgs --noheadings -o vg_name 2> /dev/null || true); do
    vgchange -an "$vg"
  done
  for md in "$dev"/md[0-9]*; do
    [ -e "$md" ] && mdadm --stop "$md"
  done
  for d in $(disks); do
    for part in "$sys/block/$d/$d"*; do
      [ -e "$part" ] || continue
      mdadm --zero-superblock "$dev/${part##*/}" 2> /dev/null || true
      wipefs -a "$dev/${part##*/}"
    done
    wipefs -a "$dev/$d"
    sgdisk --zap-all "$dev/$d"
  done
}

disk_with_serial() {
  local d
  for d in $(disks); do
    if [ "$(serial_of "$d")" = "$1" ]; then
      echo "$dev/$d"
      return
    fi
  done
  fail "no disk has serial $1"
}

join_vswitch() {
  local vlan=$1 address=$2 gateway=$3 server=$4
  ip link add link "$(public_link)" name "$vswitch_link" type vlan id "$vlan"
  ip link set "$vswitch_link" mtu "$vswitch_mtu" up
  ip addr add "$address" dev "$vswitch_link"
  ip route add "$server/32" via "$gateway" dev "$vswitch_link"
}

# fetch retries while the image server still builds the image, and stops at once on a key other than the pinned one.
# Given a disk it streams the image onto it, and otherwise it asks only for the image's headers.
fetch() {
  local url=$1 pin=$2 disk=${3:-} n=0 statuses
  local args=(--silent --show-error --fail --insecure --pinnedpubkey "$pin" --connect-timeout 10)
  while :; do
    n=$((n + 1))
    set +e
    if [ -n "$disk" ]; then
      curl "${args[@]}" "$url" | dd of="$disk" bs=4M conv=notrunc,fsync status=none
      statuses=("${PIPESTATUS[@]}")
    else
      curl "${args[@]}" --head "$url" > /dev/null
      statuses=("$?" 0)
    fi
    set -e
    [ "${statuses[*]}" = "0 0" ] && return 0
    if [ "${statuses[0]}" -eq "$pin_refused" ]; then
      fail "$url presented a key other than the pinned $pin, so nothing more is written"
    fi
    [ "$n" -lt "$attempts" ] || fail "$url gave no answer in $attempts attempts, the last ending with curl status ${statuses[0]}"
    sleep "$interval"
  done
}

# write_image clears the disks only once the image server answered with the pinned key over the vSwitch.
write_image() {
  [ "$#" -eq 7 ] || fail "usage: rescue.sh write <vlan> <address/prefix> <vswitch gateway> <image server> <port> <pin> <serial>"
  local disk tool url="https://$4:$5/image.raw"
  for tool in curl dd wipefs sgdisk mdadm; do
    command -v "$tool" > /dev/null || fail "the rescue system has no $tool, so no disk was touched"
  done
  disk=$(disk_with_serial "$7")
  join_vswitch "$1" "$2" "$3" "$4"
  fetch "$url" "$6"
  clear_disks
  fetch "$url" "$6" "$disk"
  sync
  ip link del "$vswitch_link"
  echo "rescue: wrote the image to $disk, serial $7"
}

case ${1:-} in
  read) read_hardware ;;
  write) shift && write_image "$@" ;;
  *) fail "usage: rescue.sh read | rescue.sh write ..." ;;
esac
