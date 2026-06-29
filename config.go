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
}

func defaultConfig() Config {
	return Config{
		ToggleKey: 140, // KEY_CALC / XF86Calculator
		Serial:    "/dev/ttyUSB0",
		BLE:       "hidbridge",
		Net:       "hidbridge.local:3232",
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
