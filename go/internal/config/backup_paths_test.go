package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestBackupPathsMapInternalFilesWithoutChangingIntent(t *testing.T) {
	source := filepath.Join(t.TempDir(), "old")
	target := filepath.Join(t.TempDir(), "new")
	cfg := map[string]any{
		"state":             map[string]any{"path": filepath.Join(source, "state.db"), "cold_dir": filepath.Join(source, "cold"), "backup_dir": filepath.Join(source, "backups")},
		"device_repository": map[string]any{"root_dir": filepath.Join(source, "repository"), "enabled": false},
		"nova":              map[string]any{"key_path": filepath.Join(source, "nova.key"), "enabled": false},
		"ocpp":              map[string]any{"tls": map[string]any{"cert_file": filepath.Join(source, "tls", "cert.pem"), "key_file": filepath.Join(source, "tls", "key.pem"), "client_ca_file": filepath.Join(source, "tls", "ca.pem")}, "password": "preserved"},
		"drivers":           []any{map[string]any{"lua": filepath.Join(source, "custom.lua"), "config": map[string]any{"path": "vendor value"}}},
		"future":            json.Number("9007199254740993"),
	}
	if err := relocateBackupPaths(cfg, source, target, "state.db", "config.yaml"); err != nil {
		t.Fatal(err)
	}
	if cfg["device_repository"].(map[string]any)["root_dir"] != filepath.Join(target, "repository") || cfg["nova"].(map[string]any)["key_path"] != filepath.Join(target, "nova.key") {
		t.Fatal("managed files still point at source")
	}
	tls := cfg["ocpp"].(map[string]any)["tls"].(map[string]any)
	for key, file := range map[string]string{"cert_file": "cert.pem", "key_file": "key.pem", "client_ca_file": "ca.pem"} {
		if tls[key] != filepath.Join(target, "tls", file) {
			t.Fatal("TLS file still points at source", key)
		}
	}
	driver := cfg["drivers"].([]any)[0].(map[string]any)
	if driver["lua"] != filepath.Join(target, "custom.lua") || driver["config"].(map[string]any)["path"] != "vendor value" || cfg["future"] != json.Number("9007199254740993") || cfg["ocpp"].(map[string]any)["password"] != "preserved" {
		t.Fatal("restore changed settings outside owned paths")
	}
}

func TestBackupPathsRejectUnknownExternalAndForeignRoots(t *testing.T) {
	for _, value := range []string{filepath.Join(t.TempDir(), "outside"), "../outside", `C:\FTW\cold`, `\\server\share\cold`} {
		cfg := map[string]any{"state": map[string]any{"cold_dir": value}}
		if err := relocateBackupPaths(cfg, "", t.TempDir(), "state.db", "config.yaml"); err == nil {
			t.Fatalf("unknown path silently accepted: %s", value)
		}
	}
}
