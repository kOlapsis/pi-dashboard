package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

const (
	releaseBase   = "https://github.com/kOlapsis/pi-dashboard/releases/download"
	assetName     = "pi-dashboard_linux_arm64"
	installDest   = "/usr/local/sbin/pi-dashboard-install"
	provisionDest = "/etc/pi-dashboard/provision.env"
	kioskEnvDest  = "/etc/pi-dashboard/kiosk.env"
	sessionDest   = "/usr/local/lib/pi-dashboard/kiosk-session"
)

var outputs = []string{"user-data", "network-config", "meta-data"}

var knownKeys = []string{
	"HOSTNAME", "ADMIN_USER", "SSH_PUBKEY", "WIFI_SSID", "WIFI_PSK", "WIFI_COUNTRY",
	"TIMEZONE", "LOCALE", "KEYBOARD", "CONFIG_YAML", "RELEASE_TAG", "BINARY_SHA256",
	"VIDEO_ARG", "KIOSK_MODE",
}

var fileModes = map[string]string{
	installDest:   "0755",
	sessionDest:   "0755",
	provisionDest: "0600",
	kioskEnvDest:  "0600",
}

var (
	reKey      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	reHostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	reUser     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reCountry  = regexp.MustCompile(`^[A-Z]{2}$`)
	reTimezone = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)
	reLocale   = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9-]+)?(@[a-z]+)?$`)
	reKeyboard = regexp.MustCompile(`^[a-z0-9_-]+$`)
	reTag      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	reSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reVideoArg = regexp.MustCompile(`^[A-Za-z0-9_.:=,@+-]+$`)
	reMode     = regexp.MustCompile(`^[0-9]+x[0-9]+(@[0-9]+(\.[0-9]+)?(Hz)?)?$`)
	reSSHKey   = regexp.MustCompile(`^(ssh-[a-z0-9]+|ecdsa-sha2-nistp[0-9]+|sk-[a-z0-9@.-]+) [A-Za-z0-9+/]+=*( .*)?$`)
)

type settings struct {
	Hostname   string
	AdminUser  string
	SSHKeys    []string
	SSID       string
	PSK        string
	Country    string
	Timezone   string
	Locale     string
	Keyboard   string
	Config     []byte
	ReleaseTag string
	SHA256     string
	VideoArg   string
	KioskMode  string
}

type file struct {
	Path    string
	Mode    string
	Content string
}

type templateData struct {
	Hostname   string
	Timezone   string
	Locale     string
	Keyboard   string
	AdminUser  string
	SSHKeys    []string
	SSID       string
	PSK        string
	Country    string
	Config     string
	Files      []file
	InstanceID string
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fl := flag.NewFlagSet("render", flag.ContinueOnError)
	secrets := fl.String("secrets", "pi/secrets.env", "secrets file")
	out := fl.String("out", "out/boot", "output directory")
	root := fl.String("root", "pi", "directory holding templates, install.sh and rootfs")
	if err := fl.Parse(args); err != nil {
		return err
	}
	env, err := loadEnv(*secrets)
	if err != nil {
		return err
	}
	s, err := loadSettings(env)
	if err != nil {
		return err
	}
	if s.SHA256 == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if s.SHA256, err = fetchSHA256(ctx, http.DefaultClient, releaseBase, s.ReleaseTag); err != nil {
			return err
		}
	}
	rendered, err := render(*root, s, time.Now())
	if err != nil {
		return err
	}
	if err := writeOutputs(*out, rendered); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "wrote %s/{%s} for %s (release %s, sha256 %.12s...)\n", *out, strings.Join(outputs, ","), s.Hostname, s.ReleaseTag, s.SHA256)
	return err
}

func loadEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	env, err := parseEnv(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return env, nil
}

func parseEnv(r io.Reader) (map[string]string, error) {
	env := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !reKey.MatchString(key) {
			return nil, fmt.Errorf("line %d: want KEY=VALUE", n)
		}
		if !slices.Contains(knownKeys, key) {
			return nil, fmt.Errorf("line %d: unknown key %s", n, key)
		}
		env[key] = unquote(strings.TrimSpace(val))
	}
	return env, sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func loadSettings(env map[string]string) (settings, error) {
	get := func(key, def string) string {
		if v := env[key]; v != "" {
			return v
		}
		return def
	}
	s := settings{
		Hostname:   get("HOSTNAME", "pi-dash"),
		AdminUser:  get("ADMIN_USER", "ben"),
		SSID:       env["WIFI_SSID"],
		Country:    get("WIFI_COUNTRY", "FR"),
		Timezone:   get("TIMEZONE", "Europe/Paris"),
		Locale:     get("LOCALE", "fr_FR.UTF-8"),
		Keyboard:   get("KEYBOARD", "fr"),
		ReleaseTag: env["RELEASE_TAG"],
		SHA256:     strings.ToLower(env["BINARY_SHA256"]),
		VideoArg:   env["VIDEO_ARG"],
		KioskMode:  env["KIOSK_MODE"],
	}
	if s.ReleaseTag == "" {
		return settings{}, errors.New("RELEASE_TAG is required (for example v0.1.0)")
	}
	rules := []struct {
		key      string
		value    string
		pattern  *regexp.Regexp
		optional bool
	}{
		{"HOSTNAME", s.Hostname, reHostname, false},
		{"ADMIN_USER", s.AdminUser, reUser, false},
		{"WIFI_COUNTRY", s.Country, reCountry, false},
		{"TIMEZONE", s.Timezone, reTimezone, false},
		{"LOCALE", s.Locale, reLocale, false},
		{"KEYBOARD", s.Keyboard, reKeyboard, false},
		{"RELEASE_TAG", s.ReleaseTag, reTag, false},
		{"BINARY_SHA256", s.SHA256, reSHA256, true},
		{"VIDEO_ARG", s.VideoArg, reVideoArg, true},
		{"KIOSK_MODE", s.KioskMode, reMode, true},
	}
	for _, r := range rules {
		if r.optional && r.value == "" {
			continue
		}
		if !r.pattern.MatchString(r.value) {
			return settings{}, fmt.Errorf("%s: invalid value %q", r.key, r.value)
		}
	}

	passphrase := env["WIFI_PSK"]
	if (s.SSID == "") != (passphrase == "") {
		return settings{}, errors.New("WIFI_SSID and WIFI_PSK must be set together")
	}
	if s.SSID != "" {
		psk, err := wpaPSK(s.SSID, passphrase)
		if err != nil {
			return settings{}, err
		}
		s.PSK = psk
	}

	var err error
	if s.SSHKeys, err = readKeys(expand(get("SSH_PUBKEY", "~/.ssh/benjamin_rsa.pub"))); err != nil {
		return settings{}, err
	}
	cfgPath := expand(get("CONFIG_YAML", "~/.config/pi-dashboard/config.yaml"))
	if s.Config, err = os.ReadFile(cfgPath); err != nil {
		return settings{}, err
	}
	if len(bytes.TrimSpace(s.Config)) == 0 {
		return settings{}, fmt.Errorf("%s: empty", cfgPath)
	}
	return s, nil
}

func expand(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func readKeys(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []string
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !reSSHKey.MatchString(line) {
			return nil, fmt.Errorf("%s: not a public key line", path)
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no public key", path)
	}
	return keys, nil
}

func wpaPSK(ssid, passphrase string) (string, error) {
	if n := len(ssid); n < 1 || n > 32 {
		return "", errors.New("WIFI_SSID: want 1 to 32 bytes")
	}
	if n := len(passphrase); n < 8 || n > 63 {
		return "", errors.New("WIFI_PSK: want 8 to 63 characters")
	}
	for _, r := range passphrase {
		if r < 0x20 || r > 0x7e {
			return "", errors.New("WIFI_PSK: printable ASCII only")
		}
	}
	key, err := pbkdf2.Key(sha1.New, passphrase, []byte(ssid), 4096, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func fetchSHA256(ctx context.Context, client *http.Client, base, tag string) (string, error) {
	url := base + "/" + tag + "/SHA256SUMS"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot fetch %s (set BINARY_SHA256 to skip): %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cannot fetch %s: %s (is release %s published? set BINARY_SHA256 to skip)", url, resp.Status, tag)
	}
	sum, err := findSum(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return sum, nil
}

func findSum(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != assetName {
			continue
		}
		if sum := strings.ToLower(fields[0]); reSHA256.MatchString(sum) {
			return sum, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no valid %s entry", assetName)
}

func render(root string, s settings, now time.Time) (map[string][]byte, error) {
	files, err := collectFiles(root, s)
	if err != nil {
		return nil, err
	}
	data := templateData{
		Hostname:   s.Hostname,
		Timezone:   s.Timezone,
		Locale:     s.Locale,
		Keyboard:   s.Keyboard,
		AdminUser:  s.AdminUser,
		SSHKeys:    s.SSHKeys,
		SSID:       s.SSID,
		PSK:        s.PSK,
		Country:    s.Country,
		Config:     string(s.Config),
		Files:      files,
		InstanceID: fmt.Sprintf("pi-dash-%d", now.Unix()),
	}
	funcs := template.FuncMap{"q": quote, "b64": b64, "indent": indent}
	rendered := make(map[string][]byte, len(outputs))
	for _, name := range outputs {
		src, err := os.ReadFile(filepath.Join(root, "templates", name+".tmpl"))
		if err != nil {
			return nil, err
		}
		tmpl, err := template.New(name).Funcs(funcs).Parse(string(src))
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rendered[name] = buf.Bytes()
	}
	return rendered, nil
}

func collectFiles(root string, s settings) ([]file, error) {
	install, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		return nil, err
	}
	files := []file{
		{Path: provisionDest, Content: provisionEnv(s)},
		{Path: installDest, Content: string(install)},
	}
	rootfs := filepath.Join(root, "rootfs")
	err = filepath.WalkDir(rootfs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		rel, err := filepath.Rel(rootfs, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, file{Path: "/" + filepath.ToSlash(rel), Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range files {
		f := &files[i]
		if f.Path == kioskEnvDest && s.KioskMode != "" {
			f.Content = withKioskMode(f.Content, s.KioskMode)
		}
		f.Mode = "0644"
		if m, ok := fileModes[f.Path]; ok {
			f.Mode = m
		}
		if err := checkText(f.Path, f.Content); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func provisionEnv(s settings) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"RELEASE_TAG", s.ReleaseTag},
		{"BINARY_SHA256", s.SHA256},
		{"WIFI_COUNTRY", s.Country},
		{"ADMIN_USER", s.AdminUser},
		{"VIDEO_ARG", s.VideoArg},
	} {
		fmt.Fprintf(&b, "%s='%s'\n", kv[0], kv[1])
	}
	return b.String()
}

func withKioskMode(content, mode string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, "KIOSK_MODE=") {
			lines[i] = "KIOSK_MODE=" + mode
			found = true
		}
	}
	if !found {
		lines = append(lines, "KIOSK_MODE="+mode)
	}
	return strings.Join(lines, "\n") + "\n"
}

func checkText(path, content string) error {
	switch {
	case !utf8.ValidString(content):
		return fmt.Errorf("%s: not valid UTF-8", path)
	case strings.IndexFunc(content, isForbidden) >= 0:
		return fmt.Errorf("%s: control characters other than tab and newline", path)
	case !strings.HasSuffix(content, "\n") || strings.HasSuffix(content, "\n\n"):
		return fmt.Errorf("%s: must end with exactly one newline", path)
	case content[0] == ' ' || content[0] == '\n':
		return fmt.Errorf("%s: must not start with a space or a blank line", path)
	}
	return nil
}

func isForbidden(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f
}

func writeOutputs(dir string, rendered map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range outputs {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, rendered[name], 0o600); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func quote(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
