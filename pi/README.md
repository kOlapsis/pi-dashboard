# Raspberry Pi provisioning

Turns a stock Raspberry Pi OS Lite (64-bit, Trixie) card into a kiosk: a hardened `pi-dashboard` service on 127.0.0.1:8080 and a labwc + Chromium session on tty1 showing it full screen. Everything is delivered through the three cloud-init files of the boot partition, rendered from your secrets. Nothing is piped from the network into a shell: the binary is downloaded from a pinned GitHub release and checked against a sha256.

| Path | Role |
|---|---|
| `cmd/render` | renders `user-data`, `network-config`, `meta-data` from `secrets.env` |
| `templates/` | the three cloud-init templates |
| `install.sh` | embedded in `user-data`, runs once as root on first boot |
| `rootfs/` | units, PAM stack, kiosk session, labwc, Chromium policy and apt/NetworkManager/journald drop-ins, copied verbatim to the Pi |
| `secrets.env.example` | copy to `secrets.env` (gitignored) |

## 1. Configure

`cp pi/secrets.env.example pi/secrets.env`, then edit. Format: `KEY=VALUE`, optional quotes, `#` comments only at the start of a line (a `#` inside a value is kept), unknown keys are rejected.

| Key | Default | Notes |
|---|---|---|
| `HOSTNAME` | `pi-dash` | reachable as `<hostname>.local` through avahi |
| `ADMIN_USER` | `ben` | only account: SSH key only, no password, passwordless sudo |
| `SSH_PUBKEY` | `~/.ssh/benjamin_rsa.pub` | path to a public key file, one key per line |
| `WIFI_SSID`, `WIFI_PSK` | none | set both, or neither for Ethernet only. The passphrase is stored in `network-config` as its PBKDF2-SHA1 derivation, like Imager does |
| `WIFI_COUNTRY` | `FR` | regulatory domain |
| `TIMEZONE`, `LOCALE`, `KEYBOARD` | `Europe/Paris`, `fr_FR.UTF-8`, `fr` | |
| `CONFIG_YAML` | `~/.config/pi-dashboard/config.yaml` | embedded in `user-data`, installed as `/etc/pi-dashboard/config.yaml` (0600, root) |
| `RELEASE_TAG` | required | tag of a published release that holds `pi-dashboard_linux_arm64` and `SHA256SUMS` |
| `BINARY_SHA256` | taken from the release's `SHA256SUMS` | set it to render offline |
| `VIDEO_ARG` | empty | kernel argument appended to `/boot/firmware/cmdline.txt`, see Display |
| `KIOSK_MODE` | empty | output mode passed to `wlr-randr`, written to `/etc/pi-dashboard/kiosk.env` |

## 2. Render

```
make boot-files
```

Writes `out/boot/{user-data,network-config,meta-data}` (0600, `out/` is gitignored), then validates `user-data` with `cloud-init schema` and `network-config` with `netplan generate` (cloud-init's own schema does not know the `wifis` key; on the Pi the file goes to netplan untouched). A missing tool only prints a warning. They contain the Wi-Fi key and your API tokens.

## 3. Flash

1. Raspberry Pi Imager 2.x: Raspberry Pi 5, "Raspberry Pi OS (other)", "Raspberry Pi OS Lite (64-bit)", write the card. Skip the customisation step, whatever is entered there is overwritten.
2. Re-insert the card so the `bootfs` partition mounts, then overwrite the three files: `cp out/boot/user-data out/boot/network-config out/boot/meta-data /media/$USER/bootfs/ && sync`, and eject.
3. Plug the panel into HDMI0 (the port next to USB-C) and power on.

## 4. First boot

Takes 10 to 15 minutes, mostly apt. The screen stays on a text console until the final reboot, which the Pi triggers by itself. SSH is up from the start of the install: `make ssh`, or `ssh -i ~/.ssh/benjamin_rsa ben@pi-dash.local`.

The installer (`/usr/local/sbin/pi-dashboard-install`) waits for the network and the clock, installs the packages, creates the `kiosk` user, installs the verified binary, sets the Wi-Fi country, appends `consoleblank=0` (and `VIDEO_ARG`) to `cmdline.txt`, enables `pi-dashboard` and `pi-kiosk`, replaces `user-data` and `network-config` on the boot partition by empty stubs (that partition is readable by every local user), disables cloud-init and reboots. The reboot only happens when every step succeeded. On failure the Pi stays up with SSH and the log tells why; fix the cause and run `sudo bash /usr/local/sbin/pi-dashboard-install` again (idempotent), then `sudo reboot`.

## 5. Verify

```
cloud-init status --long
sudo tail -n 30 /var/log/pi-dashboard-install.log
systemctl is-active pi-dashboard pi-kiosk
pgrep -a swayidle
loginctl list-sessions
curl -s http://127.0.0.1:8080/ | head -n 5
sudo -u kiosk env XDG_RUNTIME_DIR=/run/user/$(id -u kiosk) WAYLAND_DISPLAY=wayland-0 wlr-randr
```

During the install `cloud-init status` says `running`. After a successful run it says `disabled` (the marker file that also gates the reboot), which is expected; `error` means the installer failed. `loginctl` must list a session of `kiosk` on `seat0` and `tty1`. `wlr-randr` shows the active mode of `HDMI-A-1`.

## 6. Operate

- `make deploy` builds the arm64 binary, installs it and restarts the service. `make deploy-config` pushes `CONFIG_YAML`. `make deploy-kiosk` pushes `rootfs/usr/local/lib/pi-dashboard/kiosk-session` and restarts `pi-kiosk`. `make logs` follows both units.
- Debian security and Raspberry Pi archive updates install unattended; the Pi reboots at 04:30 when a reboot is needed. The journal is persistent and capped at 64 MB.
- A new release tag does not update an already provisioned Pi: use `make deploy`, or re-flash. Re-provisioning in place is not supported (cloud-init is disabled).
- `pi-dashboard doctor` runs every collector once: `sudo pi-dashboard doctor`.
- Screen on/off is handled by `pi-dashboard kiosk` inside the session: `journalctl -u pi-kiosk -f` shows `wake`, `night` and `wlopm` lines. `swayidle` must be installed (it is by the installer); on a Pi provisioned before it was added: `sudo apt install swayidle`, then `make deploy-kiosk`.

## Display

The mode comes from the panel EDID. Check it with the `wlr-randr` command above. If the panel advertises the wrong mode or none, force it at the kernel level, either with `VIDEO_ARG=video=HDMI-A-1:1024x600M@60D` before rendering, or on a running Pi by appending the same argument to the single line of `/boot/firmware/cmdline.txt` and rebooting. `HDMI-A-1` is HDMI0 (next to USB-C), `HDMI-A-2` is the other port; for the latter also set `KIOSK_OUTPUT` in `/etc/pi-dashboard/kiosk.env`. If the mode exists but is not selected, set `KIOSK_MODE=1024x600@60Hz` in `secrets.env` before rendering, or in `/etc/pi-dashboard/kiosk.env` followed by `sudo systemctl restart pi-kiosk`; it is applied with `wlr-randr` at session start.

Touch input is mapped to the output automatically. If it is offset or rotated, add this inside `<labwc_config>` in `/usr/local/share/pi-dashboard/labwc/rc.xml` and run `sudo systemctl restart pi-kiosk`: `<touch deviceName="" mapToOutput="HDMI-A-1" mouseEmulation="no"/>`. For a rotated output, a libinput calibration matrix is needed (see `labwc-config(5)`).

Extra Chromium flags go in `CHROMIUM_EXTRA` (`/etc/pi-dashboard/kiosk.env`). The kiosk URL is `KIOSK_URL` there: change it if `listen` in `config.yaml` is not 127.0.0.1:8080.

## Troubleshooting

- No SSH after 20 minutes: look for the Pi in the router's DHCP table (mDNS can be filtered), or plug Ethernet (DHCP, enabled on `eth0`). There is no console login (no password), so if neither network works the card has to be re-rendered and re-flashed.
- Wi-Fi does not connect: over Ethernet, `nmcli device wifi list`, `rfkill list`, `sudo rfkill unblock wifi`, `sudo raspi-config nonint do_wifi_country FR`. The profile lives in `/etc/netplan/50-cloud-init.yaml` (0600); edit it and `sudo netplan apply`.
- Installer failed: `/var/log/pi-dashboard-install.log` and `cloud-init status --long`. Usual causes: release or asset not published (404), sha256 mismatch, no route to github.com or the apt mirrors.
- Black screen after the reboot: `journalctl -u pi-kiosk -b --no-pager | tail -n 50`, `loginctl list-sessions`, `ls /dev/dri`, `sudo dmesg | grep -i -E 'drm|hdmi'`. `pi-kiosk` restarts forever every 3 s, so a persistent failure shows as a loop in the journal.
- Error page in the browser: `journalctl -u pi-dashboard -b`. A bad `config.yaml` makes the service exit and restart every 2 s; fix it and `make deploy-config`.
- Crash test: `sudo pkill -KILL -o -u kiosk chromium`. The session command exits, labwc exits with it (`-S`), systemd restarts `pi-kiosk` after 3 s on a fresh tmpfs profile, no "restore pages" bubble.

## Security notes

- `/etc/pi-dashboard/config.yaml` is root-only; the service receives it as a systemd credential and runs as a dynamic user with a strict sandbox (read-only filesystem, no capabilities, no `AF_NETLINK`, syscall allow-list). Chromium runs as `kiosk` on a throwaway tmpfs profile and only loads the local dashboard URL.
- Chromium policies disable sync, sign-in, password and autofill stores, translation and metrics. `rpi-chromium-mods` is blocked by an apt pin because it adds `--force-renderer-accessibility` and `--enable-remote-extensions`.
- The panel is driven from inside the session, not from the sandboxed service: `kiosk-session` starts `pi-dashboard kiosk` next to Chromium, which runs `swayidle` and `wlopm` with the `kiosk` user's Wayland socket. It reads `ui.idle.timeout` and `ui.night` from the dashboard API, so changing them is a `config.yaml` edit plus `sudo systemctl restart pi-dashboard`.

## Verified and assumed

Checked on the workstation: `cloud-init schema` (26.1, the Pi ships 25.2) on the rendered `user-data` and `netplan generate` on `network-config`; a PyYAML round trip of every `write_files` entry against its source file; `shellcheck`; `systemd-analyze verify` on both units (only the missing binaries are reported); the installer itself, run as root in a Debian trixie container with stubbed `systemctl`, `raspi-config`, `timedatectl` and release download (success path, second run, sha256 mismatch, retries); the unattended-upgrades origins and the apt pin syntax with apt itself; `${CREDENTIALS_DIRECTORY}` and `${STATE_DIRECTORY}` expansion in `ExecStart=` (systemd 259 here, 257 on the Pi); the labwc 0.20.2 options and `rc.xml` syntax against its man pages; the package names in the Debian trixie and Raspberry Pi archives.

Not testable without the board, so assumed: that the panel EDID advertises 1024x600; that the PAM + logind session on tty1 gives labwc the DRM and input devices; Chromium GPU rendering on Wayland on the Pi 5; the netplan/NetworkManager Wi-Fi association from the derived key; Pi OS cloud-init 25.2 honouring the `user`, `keyboard`, `locale` and `power_state` keys as 26.1 does; the 10 to 15 minute estimate.
