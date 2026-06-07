package runtime

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the Gateway's single configuration source: the selected runtime
// kind and the static set of targets it owns. Credentials live here and here
// only; they never propagate toward the BFF or the browser.
type Config struct {
	// Runtime selects the implementation. Only "qemu" is supported in the PoC.
	Runtime string `json:"runtime"`
	// Targets is the static, ordered list of backend machines.
	Targets []TargetConfig `json:"targets"`
	// Trace configures the read-only observation channel. When disabled,
	// TraceStream is refused and the runtime behaves exactly like web-terminal.
	Trace TraceConfig `json:"trace"`
}

// TraceConfig configures the read-only BehaviorEvent stream tailed from each
// guest. Like the target credentials, the watcher credentials live here and
// never propagate toward the BFF or the browser.
type TraceConfig struct {
	// Enabled exposes TraceStream for the runtime.
	Enabled bool `json:"enabled"`
	// LogPath is the guest path bloodhound writes NDJSON to.
	// Empty falls back to /var/log/bloodhound.ndjson.
	LogPath string `json:"logPath"`
	// OperatorUID is the login uid bloodhound filters on. It is the single
	// source of truth for the operator uid: provisioning derives both the guest
	// user's uid and the daemon's --uid argument from this value, so the filter
	// and the launched daemon cannot drift apart (DESIGN §14).
	OperatorUID int `json:"operatorUid"`
	// Watcher is the dedicated, non-operator account the trace tail logs in as.
	// Its login uid must differ from OperatorUID so the reader falls outside
	// bloodhound's auid filter and cannot drive the observer feedback loop
	// (DESIGN §9).
	Watcher WatcherConfig `json:"watcher"`
}

// WatcherConfig is the GW-only credential for the read-only trace connection.
type WatcherConfig struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// LogPathOrDefault returns the configured guest log path, or the default.
func (t TraceConfig) LogPathOrDefault() string {
	if t.LogPath == "" {
		return "/var/log/bloodhound.ndjson"
	}
	return t.LogPath
}

// TargetConfig fully describes how to launch and reach one backend machine.
// The Host/Port/User/Password tuple is the GW→target SSH hop; Image/Seed drive
// the QEMU launch and SSHPort is the host loopback port forwarded to guest 22.
type TargetConfig struct {
	ID    string `json:"id"`
	Label string `json:"label"`

	// SSH connection (GW → target).
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`

	// QEMU launch parameters (owned by the GW).
	Image   string `json:"image"`   // copy-on-write overlay disk
	Seed    string `json:"seed"`    // cloud-init seed image
	SSHPort int    `json:"sshPort"` // host loopback port → guest 22
	Memory  string `json:"memory"`  // e.g. "1024" (MiB); empty → default
}

// LoadConfig reads and validates a GW configuration file.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the configuration is internally consistent: a supported
// runtime, at least one target, and unique, fully specified target entries.
func (c Config) Validate() error {
	if c.Runtime == "" {
		return fmt.Errorf("config: runtime is required")
	}
	if len(c.Targets) == 0 {
		return fmt.Errorf("config: at least one target is required")
	}
	seen := make(map[string]struct{}, len(c.Targets))
	for i, t := range c.Targets {
		if t.ID == "" {
			return fmt.Errorf("config: target[%d]: id is required", i)
		}
		if _, dup := seen[t.ID]; dup {
			return fmt.Errorf("config: duplicate target id %q", t.ID)
		}
		seen[t.ID] = struct{}{}
		if t.Host == "" || t.Port == 0 {
			return fmt.Errorf("config: target %q: host and port are required", t.ID)
		}
		if t.User == "" {
			return fmt.Errorf("config: target %q: user is required", t.ID)
		}
	}
	if c.Trace.Enabled {
		if c.Trace.Watcher.User == "" {
			return fmt.Errorf("config: trace.watcher.user is required when trace is enabled")
		}
		// The watcher must be a distinct account from the operator, or its own
		// reads would be traced and amplify the observer feedback loop (§9).
		for _, t := range c.Targets {
			if c.Trace.Watcher.User == t.User {
				return fmt.Errorf("config: trace.watcher.user %q must differ from target %q's operator user", c.Trace.Watcher.User, t.ID)
			}
		}
	}
	return nil
}
