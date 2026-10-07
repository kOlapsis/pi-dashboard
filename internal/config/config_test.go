package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExampleParses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err)
	cfg, err := Parse(b)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8080", cfg.Listen)
	assert.Equal(t, time.Minute, cfg.Collectors.Mail.Interval)
	assert.Equal(t, "imap.gmail.com:993", cfg.Collectors.Mail.Host)
	assert.Equal(t, 15*time.Minute, cfg.Collectors.GitHub.Interval)
	assert.Equal(t, time.Hour, cfg.Collectors.GitHub.TrafficInterval)
	assert.Len(t, cfg.Collectors.Stripe.Accounts, 3)
	assert.Len(t, cfg.Collectors.Qonto.Orgs, 2)
	assert.Equal(t, "status", cfg.Collectors.Maintenant.Instances[0].Mode)
	assert.Equal(t, "shm", cfg.Collectors.JSONPoll[0].Name)
	assert.True(t, cfg.UI.Night.On())
	assert.True(t, cfg.UI.Night.Off())
	assert.Equal(t, 10*time.Minute, cfg.UI.Idle.Timeout)
}

func TestIdleAndScreenOff(t *testing.T) {
	cfg, err := Parse([]byte("ui: {idle: {timeout: 0s}, night: {screen_off: false}}\n"))
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), cfg.UI.Idle.Timeout)
	assert.False(t, cfg.UI.Night.Off())
}

func TestDefaultsAndMinimumInterval(t *testing.T) {
	cfg, err := Parse([]byte("collectors:\n  weather: {interval: 5s}\n  health: {sites: [{name: a, url: https://a}]}\n"))
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.Collectors.Weather.Interval)
	assert.Equal(t, "Bordeaux", cfg.Collectors.Weather.Label)
	assert.InDelta(t, 44.8378, cfg.Collectors.Weather.Lat, 1e-6)
	assert.Equal(t, 2, cfg.Collectors.Health.FailThreshold)
	assert.Nil(t, cfg.Collectors.Mail)
	assert.Equal(t, "Europe/Paris", cfg.Timezone)
	assert.Equal(t, 400, cfg.History.RetentionDays)
}

func TestEmptyFileIsValid(t *testing.T) {
	cfg, err := Parse(nil)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/pi-dashboard", cfg.DataDir)
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":        "listen: x\nbogus: 1\n",
		"bad timezone":       "timezone: Mars/Olympus\n",
		"bad night":          "ui: {night: {from: '25:00', to: '07:00'}}\n",
		"short idle":         "ui: {idle: {timeout: 10s}}\n",
		"removed panel cmd":  "ui: {night: {panel_off_cmd: [wlopm]}}\n",
		"mail missing":       "collectors: {mail: {user: a}}\n",
		"stripe no accounts": "collectors: {stripe: {accounts: []}}\n",
		"qonto incomplete":   "collectors: {qonto: {orgs: [{name: a}]}}\n",
		"registry kind":      "collectors: {registry: {items: [{kind: quay, name: x}]}}\n",
		"maintenant mode":    "collectors: {maintenant: {instances: [{name: a, url: https://a, mode: push}]}}\n",
		"jsonpoll dup":       "collectors: {jsonpoll: [{name: a, url: https://a, items: [{key: k, path: p}]}, {name: a, url: https://b, items: [{key: k, path: p}]}]}\n",
		"umami no auth":      "collectors: {umami: {base_url: https://u}}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(src))
			assert.Error(t, err)
		})
	}
}

func TestLoadWarnsOnLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("listen: 127.0.0.1:1\n"), 0o644))
	cfg, warnings, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:1", cfg.Listen)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "0600")

	require.NoError(t, os.Chmod(path, 0o600))
	_, warnings, err = Load(path)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}
