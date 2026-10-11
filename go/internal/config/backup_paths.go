package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/state"
	"gopkg.in/yaml.v3"
)

// RelocateBackup maps paths in a verified, extracted copy before activation.
// Active SQLite settings remain authoritative; unrelated fields and secrets
// stay in their original documents. Old manifests can use State.Path as the
// source root only when it names the archived database.
func RelocateBackup(staging, target, source, databaseFile, seedFile string) error {
	seedPath := filepath.Join(staging, seedFile)
	raw, err := os.ReadFile(seedPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var seed map[string]any
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &seed); err != nil {
			return fmt.Errorf("read backup config: %w", err)
		}
	}
	active := false
	if err := state.RewriteBackupConfiguration(filepath.Join(staging, databaseFile), func(doc []byte) ([]byte, error) {
		active = true
		var settings map[string]any
		decoder := json.NewDecoder(bytes.NewReader(doc))
		decoder.UseNumber()
		if err := decoder.Decode(&settings); err != nil {
			return nil, err
		}
		cfg, ok := settings["config"].(map[string]any)
		if !ok {
			return nil, errors.New("stored settings have no config")
		}
		if source == "" {
			source = backupSourceRoot(cfg, databaseFile)
		}
		if err := relocateBackupPaths(cfg, source, target, databaseFile, seedFile); err != nil {
			return nil, err
		}
		return json.Marshal(settings)
	}); err != nil {
		return err
	}
	if seed == nil && !active {
		return nil
	}
	if seed == nil {
		seed = map[string]any{}
	}
	if !active && source == "" {
		source = backupSourceRoot(seed, databaseFile)
	}
	if err := relocateBackupPaths(seed, source, target, databaseFile, seedFile); err != nil {
		return err
	}
	if active || seed["config_database"] != nil {
		ref, err := filepath.Rel(filepath.Dir(filepath.Join(target, seedFile)), filepath.Join(target, databaseFile))
		if err != nil {
			return err
		}
		seed["config_database"] = filepath.ToSlash(ref)
	}
	encoded, err := yaml.Marshal(seed)
	if err != nil {
		return err
	}
	return writeConfigAtomic(defaultDurableWriter, seedPath, encoded)
}

func backupSourceRoot(cfg map[string]any, databaseFile string) string {
	section, _ := cfg["state"].(map[string]any)
	value, _ := section["path"].(string)
	if !filepath.IsAbs(value) {
		value, _ = cfg["config_database"].(string)
	}
	if !filepath.IsAbs(value) {
		return ""
	}
	root := filepath.Clean(value)
	for range strings.Split(filepath.ToSlash(databaseFile), "/") {
		root = filepath.Dir(root)
	}
	if filepath.Join(root, databaseFile) != filepath.Clean(value) {
		return ""
	}
	return root
}

func relocateBackupPaths(cfg map[string]any, source, target, databaseFile, seedFile string) error {
	paths := [][]string{
		{"state", "path"}, {"state", "cold_dir"}, {"state", "backup_dir"},
		{"device_repository", "root_dir"}, {"nova", "key_path"},
		{"ocpp", "tls", "cert_file"}, {"ocpp", "tls", "key_file"}, {"ocpp", "tls", "client_ca_file"},
	}
	mapPath := func(value, name string) (string, error) {
		if value == "" {
			return value, nil
		}
		if name == "state.path" {
			return filepath.Join(target, databaseFile), nil
		}
		if source != "" && filepath.Clean(source) == filepath.Clean(target) {
			return value, nil
		}
		if (len(value) > 1 && value[1] == ':') || strings.HasPrefix(value, `\\`) {
			return "", fmt.Errorf("%s uses another platform's path; map it into the data directory before moving", name)
		}
		if !filepath.IsAbs(value) {
			// Cold paths historically resolve from Core's working data directory.
			if name == "state.cold_dir" {
				if !backupPathInside(".", value) {
					return "", fmt.Errorf("%s leaves the backup; copy it into the data directory before moving", name)
				}
				return filepath.Join(target, value), nil
			}
			base := filepath.Dir(seedFile)
			if name == "state.backup_dir" {
				base = filepath.Dir(databaseFile)
			}
			if !backupPathInside(".", filepath.Join(base, value)) {
				return "", fmt.Errorf("%s leaves the backup; copy it into the data directory before moving", name)
			}
			return value, nil
		}
		if source != "" && backupPathInside(source, value) {
			rel, _ := filepath.Rel(source, value)
			return filepath.Join(target, rel), nil
		}
		if source == "" && backupPathInside(target, value) {
			return value, nil
		}
		return "", fmt.Errorf("%s is outside the known backup root; copy it into the data directory before moving", name)
	}
	for _, keys := range paths {
		parent := cfg
		for _, key := range keys[:len(keys)-1] {
			parent, _ = parent[key].(map[string]any)
		}
		key := keys[len(keys)-1]
		value, ok := parent[key]
		if !ok || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s is not a path string", strings.Join(keys, "."))
		}
		mapped, err := mapPath(text, strings.Join(keys, "."))
		if err != nil {
			return err
		}
		parent[key] = mapped
	}
	if drivers, ok := cfg["drivers"].([]any); ok {
		for _, driver := range drivers {
			d, ok := driver.(map[string]any)
			if !ok {
				return errors.New("driver config is not an object")
			}
			if text, ok := d["lua"].(string); ok {
				mapped, err := mapPath(text, "drivers.lua")
				if err != nil {
					return err
				}
				d["lua"] = mapped
			}
		}
	}
	return nil
}

func backupPathInside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
