package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	testKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDummyKeyForTestsOnly0000000000000000 test@host"
	testPass = "pass word#1"
	testSSID = "Home Net"
)

var (
	piRoot = filepath.Join("..", "..")
	zeros  = strings.Repeat("0", 64)
)

type userData struct {
	Hostname       string `yaml:"hostname"`
	ManageEtcHosts bool   `yaml:"manage_etc_hosts"`
	Timezone       string `yaml:"timezone"`
	Locale         string `yaml:"locale"`
	Keyboard       struct {
		Model  string `yaml:"model"`
		Layout string `yaml:"layout"`
	} `yaml:"keyboard"`
	SSHPwauth bool `yaml:"ssh_pwauth"`
	User      struct {
		Name       string   `yaml:"name"`
		Shell      string   `yaml:"shell"`
		LockPasswd bool     `yaml:"lock_passwd"`
		Sudo       string   `yaml:"sudo"`
		Keys       []string `yaml:"ssh_authorized_keys"`
	} `yaml:"user"`
	Bootcmd    [][]string `yaml:"bootcmd"`
	WriteFiles []struct {
		Path        string `yaml:"path"`
		Owner       string `yaml:"owner"`
		Permissions string `yaml:"permissions"`
		Encoding    string `yaml:"encoding"`
		Content     string `yaml:"content"`
	} `yaml:"write_files"`
	Runcmd     [][]string `yaml:"runcmd"`
	PowerState struct {
		Mode      string   `yaml:"mode"`
		Message   string   `yaml:"message"`
		Condition []string `yaml:"condition"`
	} `yaml:"power_state"`
}

type networkConfig struct {
	Network struct {
		Version   int `yaml:"version"`
		Ethernets map[string]struct {
			DHCP4    bool `yaml:"dhcp4"`
			DHCP6    bool `yaml:"dhcp6"`
			Optional bool `yaml:"optional"`
		} `yaml:"ethernets"`
		Wifis map[string]struct {
			DHCP4        bool   `yaml:"dhcp4"`
			RegDomain    string `yaml:"regulatory-domain"`
			Optional     bool   `yaml:"optional"`
			AccessPoints map[string]struct {
				Password string `yaml:"password"`
			} `yaml:"access-points"`
		} `yaml:"wifis"`
	} `yaml:"network"`
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "id.pub"), "# comment\n"+testKey+"\n")
	write(t, filepath.Join(dir, "config.yaml"), "listen: 127.0.0.1:8080\ncollectors: {}\n")
	lines := append([]string{
		"# fixture",
		"SSH_PUBKEY=" + filepath.Join(dir, "id.pub"),
		"CONFIG_YAML=" + filepath.Join(dir, "config.yaml"),
		`WIFI_SSID="` + testSSID + `"`,
		"WIFI_PSK='" + testPass + "'",
		"RELEASE_TAG=v0.1.0",
		"BINARY_SHA256=" + zeros,
	}, extra...)
	write(t, filepath.Join(dir, "secrets.env"), strings.Join(lines, "\n")+"\n")
	return dir
}

func renderFixture(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "out", "boot")
	args := []string{"-secrets", filepath.Join(dir, "secrets.env"), "-out", out, "-root", piRoot}
	if err := run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	return out
}

func decode[T any](t *testing.T, path string) T {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(readFile(t, path)))
	dec.KnownFields(true)
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rootfsFiles(t *testing.T) map[string]string {
	t.Helper()
	base := filepath.Join(piRoot, "rootfs")
	files := map[string]string{}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		files["/"+filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestWPAPSKVector(t *testing.T) {
	got, err := wpaPSK("IEEE", "password")
	if err != nil {
		t.Fatal(err)
	}
	if want := "f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e"; got != want {
		t.Fatalf("wpaPSK = %s, want %s", got, want)
	}
}

func TestWPAPSKRejects(t *testing.T) {
	for name, c := range map[string][2]string{
		"empty ssid":       {"", "password"},
		"long ssid":        {strings.Repeat("a", 33), "password"},
		"short passphrase": {"net", "short"},
		"long passphrase":  {"net", strings.Repeat("a", 64)},
		"non ascii":        {"net", "pässword12"},
	} {
		if _, err := wpaPSK(c[0], c[1]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRenderUserData(t *testing.T) {
	dir := newFixture(t)
	out := renderFixture(t, dir)
	raw := readFile(t, filepath.Join(out, "user-data"))
	if !strings.HasPrefix(raw, "#cloud-config\n") {
		t.Fatalf("user-data must start with #cloud-config, got %q", raw[:min(20, len(raw))])
	}
	ud := decode[userData](t, filepath.Join(out, "user-data"))

	if ud.Hostname != "pi-dash" || !ud.ManageEtcHosts || ud.Timezone != "Europe/Paris" || ud.Locale != "fr_FR.UTF-8" {
		t.Errorf("unexpected system settings: %+v", ud)
	}
	if ud.Keyboard.Model != "pc105" || ud.Keyboard.Layout != "fr" || ud.SSHPwauth {
		t.Errorf("unexpected keyboard or ssh_pwauth: %+v", ud)
	}
	if ud.User.Name != "ben" || !ud.User.LockPasswd || ud.User.Sudo != "ALL=(ALL) NOPASSWD:ALL" || !slices.Equal(ud.User.Keys, []string{testKey}) {
		t.Errorf("unexpected user: %+v", ud.User)
	}
	if fmt.Sprint(ud.Bootcmd) != "[[rfkill unblock wifi]]" {
		t.Errorf("bootcmd = %v", ud.Bootcmd)
	}
	wantCmds := [][]string{{"systemctl", "enable", "--now", "ssh"}, {"bash", "/usr/local/sbin/pi-dashboard-install"}}
	if fmt.Sprint(ud.Runcmd) != fmt.Sprint(wantCmds) {
		t.Errorf("runcmd = %v", ud.Runcmd)
	}
	if ud.PowerState.Mode != "reboot" || !slices.Equal(ud.PowerState.Condition, []string{"test", "-e", "/etc/cloud/cloud-init.disabled"}) {
		t.Errorf("unexpected power_state: %+v", ud.PowerState)
	}

	wantModes := map[string]string{
		"/etc/pi-dashboard/config.yaml":             "0600",
		"/etc/pi-dashboard/provision.env":           "0600",
		"/etc/pi-dashboard/kiosk.env":               "0600",
		"/usr/local/sbin/pi-dashboard-install":      "0755",
		"/usr/local/lib/pi-dashboard/kiosk-session": "0755",
	}
	want := rootfsFiles(t)
	want["/usr/local/sbin/pi-dashboard-install"] = readFile(t, filepath.Join(piRoot, "install.sh"))
	got := map[string]string{}
	for _, f := range ud.WriteFiles {
		if f.Owner != "root:root" {
			t.Errorf("%s: owner %q", f.Path, f.Owner)
		}
		mode := wantModes[f.Path]
		if mode == "" {
			mode = "0644"
		}
		if f.Permissions != mode {
			t.Errorf("%s: permissions %q, want %q", f.Path, f.Permissions, mode)
		}
		switch f.Path {
		case "/etc/pi-dashboard/config.yaml":
			b, err := base64.StdEncoding.DecodeString(f.Content)
			if f.Encoding != "b64" || err != nil || string(b) != "listen: 127.0.0.1:8080\ncollectors: {}\n" {
				t.Errorf("config.yaml: encoding %q, err %v, content %q", f.Encoding, err, b)
			}
		case "/etc/pi-dashboard/provision.env":
			wantEnv := "RELEASE_TAG='v0.1.0'\nBINARY_SHA256='" + zeros + "'\nWIFI_COUNTRY='FR'\nADMIN_USER='ben'\nVIDEO_ARG=''\n"
			if f.Content != wantEnv {
				t.Errorf("provision.env = %q", f.Content)
			}
		default:
			got[f.Path] = f.Content
		}
	}
	for path, content := range want {
		if g, ok := got[path]; !ok {
			t.Errorf("%s missing from write_files", path)
		} else if g != content {
			t.Errorf("%s: content differs from the source file", path)
		}
		delete(got, path)
	}
	for path := range got {
		t.Errorf("unexpected write_files entry %s", path)
	}

	for _, path := range []string{
		"/etc/systemd/system/pi-dashboard.service",
		"/etc/systemd/system/pi-kiosk.service",
		"/etc/pam.d/pi-kiosk",
		"/etc/pi-dashboard/kiosk.env",
		"/etc/chromium/policies/managed/pi-dashboard.json",
		"/etc/NetworkManager/conf.d/20-wifi-powersave-off.conf",
		"/etc/systemd/journald.conf.d/50-pi-dashboard.conf",
		"/etc/apt/apt.conf.d/52pi-dashboard-unattended",
		"/usr/local/lib/pi-dashboard/kiosk-session",
		"/usr/local/share/pi-dashboard/labwc/rc.xml",
	} {
		if _, ok := want[path]; !ok {
			t.Errorf("%s is not provisioned", path)
		}
	}
}

func TestRenderOutputModes(t *testing.T) {
	out := renderFixture(t, newFixture(t))
	for _, name := range outputs {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestRenderKioskModeAndVideoArg(t *testing.T) {
	dir := newFixture(t, "KIOSK_MODE=1024x600@60Hz", "VIDEO_ARG=video=HDMI-A-1:1024x600M@60D")
	ud := decode[userData](t, filepath.Join(renderFixture(t, dir), "user-data"))
	var kiosk, provision string
	for _, f := range ud.WriteFiles {
		switch f.Path {
		case "/etc/pi-dashboard/kiosk.env":
			kiosk = f.Content
		case "/etc/pi-dashboard/provision.env":
			provision = f.Content
		}
	}
	src := rootfsFiles(t)["/etc/pi-dashboard/kiosk.env"]
	if want := strings.Replace(src, "KIOSK_MODE=\n", "KIOSK_MODE=1024x600@60Hz\n", 1); kiosk != want || kiosk == src {
		t.Errorf("kiosk.env = %q", kiosk)
	}
	if !strings.Contains(provision, "VIDEO_ARG='video=HDMI-A-1:1024x600M@60D'\n") {
		t.Errorf("provision.env = %q", provision)
	}
}

func TestRenderNetworkConfig(t *testing.T) {
	dir := newFixture(t, "WIFI_COUNTRY=GB")
	nc := decode[networkConfig](t, filepath.Join(renderFixture(t, dir), "network-config"))
	if nc.Network.Version != 2 {
		t.Errorf("version = %d", nc.Network.Version)
	}
	if eth := nc.Network.Ethernets["eth0"]; !eth.DHCP4 || !eth.DHCP6 || !eth.Optional {
		t.Errorf("eth0 = %+v", eth)
	}
	wlan := nc.Network.Wifis["wlan0"]
	if !wlan.DHCP4 || !wlan.Optional || wlan.RegDomain != "GB" {
		t.Errorf("wlan0 = %+v", wlan)
	}
	wantPSK, err := wpaPSK(testSSID, testPass)
	if err != nil {
		t.Fatal(err)
	}
	if got := wlan.AccessPoints[testSSID].Password; got != wantPSK {
		t.Errorf("password = %q, want %q", got, wantPSK)
	}
}

func TestRenderWithoutWifi(t *testing.T) {
	dir := newFixture(t, "WIFI_SSID=", "WIFI_PSK=")
	nc := decode[networkConfig](t, filepath.Join(renderFixture(t, dir), "network-config"))
	if len(nc.Network.Wifis) != 0 || len(nc.Network.Ethernets) != 1 {
		t.Errorf("unexpected network: %+v", nc.Network)
	}
}

func TestRenderMetaData(t *testing.T) {
	dir := newFixture(t)
	env, err := loadEnv(filepath.Join(dir, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadSettings(env)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := render(piRoot, s, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rendered["meta-data"]); got != "instance-id: pi-dash-1700000000\n" {
		t.Errorf("meta-data = %q", got)
	}
}

func TestParseEnv(t *testing.T) {
	in := strings.Join([]string{
		"# comment",
		"",
		"HOSTNAME=plain",
		`WIFI_SSID="double quoted"`,
		"WIFI_PSK='single # quoted'",
		"export TIMEZONE=Europe/Paris",
		"LOCALE = fr_FR.UTF-8  ",
		`VIDEO_ARG="unbalanced`,
	}, "\r\n")
	env, err := parseEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HOSTNAME":  "plain",
		"WIFI_SSID": "double quoted",
		"WIFI_PSK":  "single # quoted",
		"TIMEZONE":  "Europe/Paris",
		"LOCALE":    "fr_FR.UTF-8",
		"VIDEO_ARG": `"unbalanced`,
	}
	if fmt.Sprint(env) != fmt.Sprint(want) {
		t.Errorf("env = %v, want %v", env, want)
	}
	for _, bad := range []string{"NOEQUALS", "lower=case", "UNKNOWN_KEY=1", "=value"} {
		if _, err := parseEnv(strings.NewReader(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestSettingsErrors(t *testing.T) {
	for name, c := range map[string]struct {
		extra []string
		want  string
	}{
		"missing tag":         {[]string{"RELEASE_TAG="}, "RELEASE_TAG is required"},
		"bad tag":             {[]string{"RELEASE_TAG=v1;rm"}, "RELEASE_TAG"},
		"bad sha":             {[]string{"BINARY_SHA256=abc"}, "BINARY_SHA256"},
		"bad hostname":        {[]string{"HOSTNAME=-bad-"}, "HOSTNAME"},
		"bad user":            {[]string{"ADMIN_USER=Root"}, "ADMIN_USER"},
		"bad country":         {[]string{"WIFI_COUNTRY=fra"}, "WIFI_COUNTRY"},
		"bad video arg":       {[]string{"VIDEO_ARG=a b"}, "VIDEO_ARG"},
		"bad kiosk mode":      {[]string{"KIOSK_MODE=big"}, "KIOSK_MODE"},
		"ssid without psk":    {[]string{"WIFI_PSK="}, "must be set together"},
		"short psk":           {[]string{"WIFI_PSK=short"}, "WIFI_PSK"},
		"missing key file":    {[]string{"SSH_PUBKEY=/nonexistent/key.pub"}, "no such file"},
		"missing config file": {[]string{"CONFIG_YAML=/nonexistent/config.yaml"}, "no such file"},
	} {
		dir := newFixture(t, c.extra...)
		err := run([]string{"-secrets", filepath.Join(dir, "secrets.env"), "-out", filepath.Join(dir, "out"), "-root", piRoot}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
}

func TestRejectsPrivateKeyAndEmptyConfig(t *testing.T) {
	dir := newFixture(t)
	write(t, filepath.Join(dir, "id.pub"), "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n")
	if _, err := readKeys(filepath.Join(dir, "id.pub")); err == nil {
		t.Error("a private key must be rejected")
	}
	write(t, filepath.Join(dir, "config.yaml"), "\n  \n")
	env, err := loadEnv(filepath.Join(dir, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(env); err == nil {
		t.Error("an empty config must be rejected")
	}
}

func TestCheckText(t *testing.T) {
	for name, c := range map[string]struct {
		in string
		ok bool
	}{
		"plain":            {"a\n", true},
		"tab inside":       {"a\n\tb\n", true},
		"no newline":       {"a", false},
		"double newline":   {"a\n\n", false},
		"leading space":    {" a\n", false},
		"leading newline":  {"\na\n", false},
		"carriage return":  {"a\r\n", false},
		"escape character": {"a\x1b\n", false},
		"invalid utf8":     {"a\xff\n", false},
	} {
		if err := checkText("/x", c.in); (err == nil) != c.ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestFetchSHA256(t *testing.T) {
	sums := strings.Repeat("a", 64) + "  other_file\n" + strings.Repeat("B", 64) + " *" + assetName + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0.1.0/SHA256SUMS":
			_, _ = io.WriteString(w, sums)
		case "/v0.2.0/SHA256SUMS":
			_, _ = io.WriteString(w, strings.Repeat("a", 64)+"  other_file\n")
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()

	got, err := fetchSHA256(ctx, srv.Client(), srv.URL, "v0.1.0")
	if err != nil || got != strings.Repeat("b", 64) {
		t.Errorf("fetchSHA256 = %q, %v", got, err)
	}
	if _, err := fetchSHA256(ctx, srv.Client(), srv.URL, "v0.2.0"); err == nil || !strings.Contains(err.Error(), "no valid") {
		t.Errorf("missing entry: err = %v", err)
	}
	if _, err := fetchSHA256(ctx, srv.Client(), srv.URL, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unpublished release: err = %v", err)
	}
	srv.Close()
	if _, err := fetchSHA256(ctx, srv.Client(), srv.URL, "v0.1.0"); err == nil || !strings.Contains(err.Error(), "BINARY_SHA256") {
		t.Errorf("unreachable server: err = %v", err)
	}
}

func TestTemplateFuncs(t *testing.T) {
	if got := quote(`a&<"b"\`); got != `"a&<\"b\"\\"` {
		t.Errorf("quote = %s", got)
	}
	if got := indent(2, "a\n\nb\n"); got != "  a\n\n  b" {
		t.Errorf("indent = %q", got)
	}
	if got := b64("hi"); got != "aGk=" {
		t.Errorf("b64 = %q", got)
	}
}

func TestExpand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := expand("~/x/y"); got != filepath.Join(home, "x", "y") {
		t.Errorf("expand(~/x/y) = %s", got)
	}
	if got := expand("$HOME/z"); got != filepath.Join(home, "z") {
		t.Errorf("expand($HOME/z) = %s", got)
	}
	if got := expand("/abs/path"); got != "/abs/path" {
		t.Errorf("expand(/abs/path) = %s", got)
	}
}
