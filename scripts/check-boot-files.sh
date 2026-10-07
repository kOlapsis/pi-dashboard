#!/bin/sh
set -eu

dir=${1:-out/boot}

if command -v cloud-init >/dev/null 2>&1; then
	cloud-init schema -c "$dir/user-data"
else
	echo "warning: cloud-init not found, skipping schema validation of $dir/user-data" >&2
fi

# cloud-init's v2 schema rejects 'wifis'; the Pi hands network-config to netplan untouched.
if command -v netplan >/dev/null 2>&1; then
	root=$(mktemp -d)
	trap 'rm -rf "$root"' EXIT
	mkdir -p "$root/etc/netplan"
	install -m0600 "$dir/network-config" "$root/etc/netplan/50-cloud-init.yaml"
	netplan generate --root-dir "$root"
else
	echo "warning: netplan not found, skipping validation of $dir/network-config" >&2
fi
