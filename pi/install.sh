#!/bin/bash
set -euo pipefail

LOG=/var/log/pi-dashboard-install.log
ASSET=pi-dashboard_linux_arm64
BIN=/usr/local/bin/pi-dashboard
CMDLINE=/boot/firmware/cmdline.txt
PACKAGES=(labwc wlr-randr wlopm swayidle chromium libgl1-mesa-dri libpam-systemd fonts-liberation fonts-noto-color-emoji unattended-upgrades)
APT=(apt-get -o Acquire::Retries=5 -o Acquire::Check-Date=false -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold)

export DEBIAN_FRONTEND=noninteractive

log() {
	printf '%s %s\n' "$(date -Is)" "$*"
}

retry() {
	local attempt
	for attempt in 1 2 3 4 5 6; do
		"$@" && return 0
		log "attempt $attempt/6 failed: $*"
		[ "$attempt" -eq 6 ] || sleep $((attempt * 10))
	done
	return 1
}

wait_for_clock() {
	local deadline=$((SECONDS + 120))
	while [ "$SECONDS" -lt "$deadline" ]; do
		[ "$(timedatectl show -p NTPSynchronized --value)" = yes ] && return 0
		sleep 1
	done
	log "clock not synchronised after 120 s, continuing"
}

add_cmdline_arg() {
	local line
	line=$(head -n1 "$CMDLINE")
	case " $line " in
	*" $1 "*) return 0 ;;
	esac
	printf '%s %s\n' "${line%"${line##*[![:space:]]}"}" "$1" >"$CMDLINE"
}

install_binary() {
	local tmp url=https://github.com/kOlapsis/pi-dashboard/releases/download/$RELEASE_TAG/$ASSET
	if [ "$(sha256sum "$BIN" 2>/dev/null | cut -d' ' -f1)" = "$BINARY_SHA256" ]; then
		log "$BIN already matches $RELEASE_TAG"
	else
		tmp=$(mktemp -d)
		retry curl -fsSL --max-time 300 -o "$tmp/$ASSET" "$url"
		(cd "$tmp" && echo "$BINARY_SHA256  $ASSET" | sha256sum -c -)
		install -m0755 "$tmp/$ASSET" "$BIN"
		rm -rf "$tmp"
	fi
	"$BIN" version
}

write_sudoers() {
	local tmp
	tmp=$(mktemp)
	printf '%s ALL=(ALL) NOPASSWD: ALL\n' "$ADMIN_USER" >"$tmp"
	visudo -cf "$tmp"
	install -m0440 -o root -g root "$tmp" "/etc/sudoers.d/010_${ADMIN_USER}-nopasswd"
	rm -f "$tmp"
}

main() {
	# shellcheck source=/dev/null
	. /etc/pi-dashboard/provision.env

	log "provisioning pi-dashboard $RELEASE_TAG"
	retry curl -fsSI --max-time 20 -o /dev/null https://github.com
	wait_for_clock

	retry "${APT[@]}" update
	retry "${APT[@]}" install -y "${PACKAGES[@]}"
	"${APT[@]}" purge -y rpi-chromium-mods || true

	id kiosk >/dev/null 2>&1 || useradd --system -m -s /usr/sbin/nologin -G video,render,input kiosk

	install_binary

	raspi-config nonint do_wifi_country "$WIFI_COUNTRY" || log "do_wifi_country failed, continuing"

	add_cmdline_arg consoleblank=0
	[ -z "${VIDEO_ARG:-}" ] || add_cmdline_arg "$VIDEO_ARG"

	write_sudoers

	systemctl daemon-reload
	systemctl enable pi-dashboard.service pi-kiosk.service
	systemctl enable --now avahi-daemon.service

	printf '#cloud-config\n' >/boot/firmware/user-data
	printf 'network:\n  version: 2\n' >/boot/firmware/network-config
	sync
	touch /etc/cloud/cloud-init.disabled
	log "provisioning complete"
}

main 2>&1 | tee -a "$LOG"
