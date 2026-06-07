package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
	  "runtime": "qemu",
	  "targets": [
	    {"id":"vm1","label":"VM 1","host":"127.0.0.1","port":2222,"user":"poc","password":"poc"}
	  ]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Runtime != "qemu" || len(cfg.Targets) != 1 || cfg.Targets[0].ID != "vm1" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLogPathOrDefault(t *testing.T) {
	if got := (TraceConfig{}).LogPathOrDefault(); got != "/var/log/bloodhound.ndjson" {
		t.Fatalf("default log path = %q", got)
	}
	if got := (TraceConfig{LogPath: "/tmp/x.ndjson"}).LogPathOrDefault(); got != "/tmp/x.ndjson" {
		t.Fatalf("explicit log path = %q", got)
	}
}

func TestValidateAcceptsEnabledTrace(t *testing.T) {
	cfg := Config{
		Runtime: "qemu",
		Targets: []TargetConfig{{ID: "a", Host: "h", Port: 1, User: "poc"}},
		Trace:   TraceConfig{Enabled: true, OperatorUID: 1000, Watcher: WatcherConfig{User: "watcher", Password: "w"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid trace config, got %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]Config{
		"no runtime": {Targets: []TargetConfig{{ID: "a", Host: "h", Port: 1, User: "u"}}},
		"no targets": {Runtime: "qemu"},
		"missing id": {Runtime: "qemu", Targets: []TargetConfig{{Host: "h", Port: 1, User: "u"}}},
		"duplicate id": {Runtime: "qemu", Targets: []TargetConfig{
			{ID: "a", Host: "h", Port: 1, User: "u"},
			{ID: "a", Host: "h", Port: 2, User: "u"},
		}},
		"missing host": {Runtime: "qemu", Targets: []TargetConfig{{ID: "a", Port: 1, User: "u"}}},
		"missing user": {Runtime: "qemu", Targets: []TargetConfig{{ID: "a", Host: "h", Port: 1}}},
		"trace enabled without watcher": {
			Runtime: "qemu",
			Targets: []TargetConfig{{ID: "a", Host: "h", Port: 1, User: "poc"}},
			Trace:   TraceConfig{Enabled: true},
		},
		"watcher equals operator": {
			Runtime: "qemu",
			Targets: []TargetConfig{{ID: "a", Host: "h", Port: 1, User: "poc"}},
			Trace:   TraceConfig{Enabled: true, Watcher: WatcherConfig{User: "poc"}},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}
