package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Config holds the machine-specific settings, loaded from JSON. See
// config.example.json. Anything omitted in the file keeps its built-in default.
type Config struct {
	// Devices is the allowlist of input devices to capture/grab, matched by the
	// exact device name (EVIOCGNAME). Empty => nothing is grabbed.
	Devices []string `json:"devices"`
	// ToggleKey is the evdev keycode that toggles KVM mode (140 = KEY_CALC).
	ToggleKey uint16 `json:"toggleKey"`
	// InvertScroll inverts the vertical wheel direction.
	InvertScroll bool `json:"invertScroll"`
	// Transport defaults used by the bare -serial / -ble / -net flags.
	Serial string `json:"serial"`
	BLE    string `json:"ble"`
	Net    string `json:"net"`
	// OTAToken is the shared secret sent with -ota firmware updates; it must
	// match the board's OTA_TOKEN (.env / Kconfig).
	OTAToken string `json:"otaToken"`
	// MQTT, when Broker is set, connects the daemon to a broker so Home
	// Assistant can toggle the KVM grab (in addition to the local toggleKey)
	// and see its state. Empty Broker disables the whole MQTT path.
	MQTT MQTTConfig `json:"mqtt"`
}

// MQTTConfig describes the Home Assistant / MQTT integration. Only Broker is
// required; everything else has a sensible default (filled by withDefaults).
type MQTTConfig struct {
	Broker          string `json:"broker"`          // tcp://host:1883 (or tls://, ws://)
	User            string `json:"user"`            // optional
	Password        string `json:"password"`        // optional
	ClientID        string `json:"clientID"`        // default "hidbridge-<node>"
	BaseTopic       string `json:"baseTopic"`       // default "hidbridge"
	DiscoveryPrefix string `json:"discoveryPrefix"` // default "homeassistant"
	NodeID          string `json:"nodeID"`          // default hostname; unique per machine
	DeviceName      string `json:"deviceName"`      // default "HID Bridge"
}

// withDefaults fills empty optional fields. NodeID/ClientID derive from the
// hostname so two machines on one broker don't collide.
func (m MQTTConfig) withDefaults() MQTTConfig {
	host, _ := os.Hostname()
	if host == "" {
		host = "host"
	}
	if m.NodeID == "" {
		m.NodeID = sanitizeID(host)
	}
	if m.ClientID == "" {
		m.ClientID = "hidbridge-" + m.NodeID
	}
	if m.BaseTopic == "" {
		m.BaseTopic = "hidbridge"
	}
	if m.DiscoveryPrefix == "" {
		m.DiscoveryPrefix = "homeassistant"
	}
	if m.DeviceName == "" {
		m.DeviceName = "HID Bridge"
	}
	return m
}

// sanitizeID keeps only chars safe for MQTT topics / HA unique_ids.
func sanitizeID(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b = append(b, r)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "host"
	}
	return string(b)
}

func defaultConfig() Config {
	return Config{
		ToggleKey: 140, // KEY_CALC / XF86Calculator
		Serial:    "/dev/ttyUSB0",
		BLE:       "hidbridge",
		Net:       "hidbridge.local:3232",
		OTAToken:  "changeme",
	}
}

// loadConfig starts from defaults and overlays JSON from path. If path is empty,
// it searches ./config.json then $XDG_CONFIG_HOME/hidbridge/config.json. Returns
// the config and the file actually used ("" if none).
func loadConfig(path string) (Config, string, error) {
	cfg := defaultConfig()
	if path == "" {
		for _, p := range configSearchPaths() {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return cfg, "", nil // no file: built-in defaults (Devices empty)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, path, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, path, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, path, nil
}

func configSearchPaths() []string {
	paths := []string{"config.json"}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config")
		}
	}
	if dir != "" {
		paths = append(paths, filepath.Join(dir, "hidbridge", "config.json"))
	}
	return paths
}

func (c *Config) wants(name string) bool {
	return slices.Contains(c.Devices, name)
}
