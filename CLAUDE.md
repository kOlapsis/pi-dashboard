# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A single static Go binary (`cmd/pi-dashboard`) that polls external services and renders a French-language kiosk page on a Raspberry Pi 5 (1024×600). Go 1.26, module `github.com/kolapsis/pi-dashboard`. No frameworks, no SDKs: every source is called with plain `net/http` through `internal/httpx`.

## Commands

```sh
make run                                  # demo mode, no config needed, http://127.0.0.1:8080
make check                                # vet + golangci-lint + go test -race + CGO check (what CI runs)
go test -race ./...                       # all tests
go test -race ./internal/collector/stripe -run TestCollect_MonthBoundary   # one test
go test ./internal/collector/github -update   # rewrite golden files (also umami)
make build-arm64                          # out/pi-dashboard_linux_arm64 + SHA256SUMS
make deploy HOST=pi-dash.local            # scp + install + restart on the Pi
make deploy-config                        # push ~/.config/pi-dashboard/config.yaml to the Pi
make boot-files                           # render cloud-init files from pi/secrets.env into out/boot
pi-dashboard doctor --config config.yaml --only umami,qonto --json   # run collectors once, print OK/KO
pi-dashboard serve --demo --demo-fail=stripe --demo-stale=qonto      # degraded states in demo
pi-dashboard serve --demo --static-dir internal/web/static           # edit the UI without rebuilding
pi-dashboard kiosk --url http://127.0.0.1:8080/ --output HDMI-A-1    # screen on/off sidecar, runs inside the Pi session
```

Lint config is `.golangci.yml` (v2, standard set + errorlint, gocritic, misspell, unconvert, unparam). CI also runs `shellcheck` on `scripts/check-cgo.sh`, `pi/install.sh` and `pi/rootfs/usr/local/lib/pi-dashboard/kiosk-session`.

## Hard constraint: CGO_ENABLED=0

The binary is cross-compiled for arm64 with CGO disabled. `scripts/check-cgo.sh` fails the build if any third-party dependency has cgo files. SQLite is `modernc.org/sqlite` (pure Go) for this reason. Do not add a dependency that needs cgo.

## Architecture

```
collectors ──▶ sched ──▶ Snapshot ──▶ httpapi (/api/state, /api/events SSE) ──▶ web/static (embedded)
                 │
                 └──▶ store.History (SQLite, hourly buckets) + snapshot.json (warm restart)
```

- **`internal/collector`**: the `Collector` interface (`Name`, `Interval`, `Collect(ctx) (any, error)`), optional `Summarizer` (one-line for `doctor`), `Timeouter` (overrides the scheduler's 20 s default) and `Notifier` (`Notify(prev, cur any) []string`: labels for changes that deserve attention, implemented by mail, stripe, qonto and health as a package-level `Notify` so the demo can reuse it). `Deps` carries the shared HTTP client, `clock.Clock`, `store.History`, logger and `*time.Location`. Helpers `Delta`, `Series`, `Record` wrap history for sparklines and deltas.
- **One package per source** under `internal/collector/<name>/`: `collector.go` (New, Name, Interval, Collect, Summary), `types.go` (the `Data` struct returned by `Collect`, JSON tags consumed by `app.js`), sometimes `api.go`/`backfill.go`/`format.go`. The collector keeps a `baseURL` field so tests can point it at `httptest`.
- **`internal/sched`**: one goroutine per collector, jittered intervals, exponential backoff capped at 15 min, panic recovery. On failure the entry goes `stale` (last good data kept) while within `max(3×interval, 15 min)` of the last success, then `error`. `Restore` seeds entries from the previous `snapshot.json` so the screen is never blank after a restart. After each success it calls the collector's `Notify` with the previous typed value (decoded from the restored JSON when needed) and appends the labels to `Snapshot.Events` (last 20, monotonic `Seq`).
- **`internal/store`**: `History` interface with two implementations, `DB` (SQLite, WAL, single connection) and `Mem` (demo, doctor, tests). Points are truncated to `Bucket` (1 h); `At` looks ±36 h around the requested time.
- **`internal/config`**: YAML with `KnownFields(true)` (unknown keys are fatal). A collector is enabled by the presence of its section (pointer non-nil). `applyDefaults` sets per-collector default intervals and clamps to 30 s minimum; `Validate` collects all errors at once.
- **`internal/httpapi`**: `GET /api/state` (JSON), `GET /api/events` (SSE, same payload on every scheduler change, coalesced 250 ms), `/healthz`, static files. CSP is `default-src 'self'`: no inline scripts, no external resources.
- **`internal/web/static`**: `index.html`, `app.js`, `style.css`, fonts. Vanilla JS, no build step. `app.js` renders tiles by looking up `state.collectors[<name>]`; the "Traction" tile merges `registry`, `maintenant`, `github` and the `shm` jsonpoll. New entries in `state.events` show a toast for 20 s.
- **`internal/clock`**: `Clock` interface with `Real` and `Fake` (`Advance`, `Pending`). Everything time-dependent takes a `clock.Clock` so tests are deterministic.
- **`internal/night`**: `Window` parsing and `Active(now)`. UI dimming happens in `app.js` from `state.ui.night`; the panel itself is driven by the kiosk sidecar.
- **`internal/kiosk`**: the `pi-dashboard kiosk` sidecar, started by `pi/rootfs/.../kiosk-session` next to Chromium inside the labwc session (it needs the session's Wayland socket, which the sandboxed `pi-dashboard` service does not have). It follows `/api/events`, keeps a `swayidle` child armed with `ui.idle.timeout` (`wlopm --off` on timeout, `--on` on touch), turns the panel off for the `ui.night` window when `screen_off` is set, and on a new fresh event outside the night window runs `wlopm --on` and re-arms `swayidle`. `Machine` is the testable core, `Runner` abstracts the commands.

### Collector name contract

A collector's `Name()` must match four places: the YAML key in `config.Collectors`, the wiring in `cmd/pi-dashboard/collectors.go`, the demo generator in `internal/collector/demo/demo.go` (`mk("<name>", …)` and the `COLLECTOR_ORDER` / `entryOf(st, "<name>")` lookups in `app.js`. `jsonpoll` collectors use their configured `name` (the example config names one `shm`, which `app.js` expects).

### Adding a collector

1. `internal/config/config.go`: add the struct, the pointer field in `Collectors`, defaults in `applyDefaults`, checks in `Validate`.
2. `internal/collector/<name>/`: `collector.go`, `types.go`, `collector_test.go` with `httptest` fixtures under `testdata/`.
3. `cmd/pi-dashboard/collectors.go`: wire it.
4. `internal/collector/demo/demo.go`: add a generator so `make run` shows it.
5. `app.js` / `index.html`: a tile or a slot in an existing tile. Keep `config.example.yaml` and the README table in sync.

## Testing conventions

- `testify` (`require` for setup, `assert` for checks). Collectors are tested against `httptest.NewServer` with JSON fixtures in `testdata/`, a `clock.NewFake(...)` and `store.NewMem()`. Logs go to `io.Discard` or a `syncBuffer` when the test asserts on them.
- Fixed test time is `2026-10-07` in `Europe/Paris`; `github` and `umami` use golden JSON files (`testdata/collect.golden.json`, regenerate with `-update`).
- Each collector test asserts the interfaces at compile time: `var _ collector.Collector = (*Collector)(nil)`.
- The scheduler and store tests use `clock.Fake.Advance` rather than sleeping.

## Error and secret hygiene

- Wrap HTTP errors through `httpx.Do`/`httpx.GetJSON`: they cap the body at 8 MiB, turn non-2xx into `*httpx.StatusError` and redact query strings and user info from URLs. Never put a token, key or full URL with query into an error or a log line.
- The config file is root-only on the Pi and reaches the service through systemd `LoadCredential`; `config.Load` warns when the mode is not 0600.

## Raspberry Pi setup (`pi/`)

The Pi is provisioned once, offline, through cloud-init. Nothing is installed by hand on the board. There are two distinct flows: **first install** (flash a card) and **updates** (`make deploy` over SSH).

### First install, step by step

1. **Publish a release.** Push a tag `v*`. `.github/workflows/release.yml` builds `pi-dashboard_linux_arm64` with `CGO_ENABLED=0` and attaches it plus `SHA256SUMS` to a GitHub Release. The Pi downloads this asset during install, so the tag must exist before step 3.
2. **Fill `pi/secrets.env`.** Copy `pi/secrets.env.example` (the real file is gitignored). Required: `RELEASE_TAG` (the tag from step 1), `SSH_PUBKEY` (path to a public key), `CONFIG_YAML` (path to the dashboard `config.yaml`, default `~/.config/pi-dashboard/config.yaml`). Optional: `WIFI_SSID`/`WIFI_PSK` (both or neither), `HOSTNAME` (default `pi-dash`), `ADMIN_USER` (default `ben`), `VIDEO_ARG`, `KIOSK_MODE`.
3. **Render the boot files:** `make boot-files`. This runs `go run ./pi/cmd/render`, which reads `secrets.env`, fetches `SHA256SUMS` from the release to pin the binary hash, embeds `config.yaml` and every file under `pi/rootfs/` into the templates of `pi/templates/`, and writes `out/boot/{user-data,network-config,meta-data}`. `scripts/check-boot-files.sh` then validates `user-data` with `cloud-init schema` and `network-config` with `netplan generate` (cloud-init's schema rejects `wifis`; netplan is the real consumer on the Pi). A missing tool only prints a warning, the files stay usable. These files contain the Wi-Fi key and all API tokens.
4. **Flash the card** with Raspberry Pi Imager: Raspberry Pi OS Lite 64-bit (Trixie), skip Imager's customisation. Re-mount the card and copy the three files from `out/boot/` onto the `bootfs` partition, overwriting the existing ones.
5. **First boot** (10 to 15 min, text console). cloud-init applies `user-data`: creates the admin user (SSH key only, no password), writes `/etc/pi-dashboard/config.yaml` (root, 0600) and the `rootfs/` files, enables SSH, then runs `/usr/local/sbin/pi-dashboard-install` (that is `pi/install.sh`). The installer waits for network and clock, `apt install`s labwc, Chromium and friends, creates the `kiosk` user, downloads the release binary and checks its sha256, sets the Wi-Fi country, appends `consoleblank=0` to `cmdline.txt`, enables `pi-dashboard.service` and `pi-kiosk.service`, blanks `user-data`/`network-config` on the boot partition (readable by every local user), disables cloud-init and reboots. The reboot is gated on success: on failure the Pi stays up with SSH, see `/var/log/pi-dashboard-install.log`, fix, rerun the installer (idempotent), `sudo reboot`.
6. **After the reboot** the Pi runs two systemd units: `pi-dashboard` (the Go binary as a sandboxed dynamic user on 127.0.0.1:8080, config passed via `LoadCredential`) and `pi-kiosk` (labwc + Chromium as user `kiosk` on tty1, full screen on the local URL, restarted every 3 s if it dies). Verify with `systemctl is-active pi-dashboard pi-kiosk` and `curl -s http://127.0.0.1:8080/`.

### Updating a provisioned Pi

cloud-init is disabled after the first boot, so a new release tag changes nothing on the board. Use SSH instead (`HOST`, `PIUSER`, `SSH_KEY` are Makefile variables):

- `make deploy`: cross-compiles arm64, scp, `install` into `/usr/local/bin`, restarts `pi-dashboard`.
- `make deploy-config`: pushes `CONFIG_YAML` to `/etc/pi-dashboard/config.yaml` (0600 root) and restarts.
- `make logs` follows both units; `make ssh` opens a shell; `sudo pi-dashboard doctor` tests every collector on the Pi.

Changing anything under `pi/rootfs/` (units, kiosk session, Chromium policy) is not deployed by `make deploy`; either copy the file by hand over SSH or re-flash. Display, touch and Wi-Fi troubleshooting is in `pi/README.md`.
