// Package config owns Tendrils' on-disk state directory: the device key, the
// local config file, and the index database live here so the daemon can resume
// after a reboot without the owner re-entering anything.
//
// The local config file is authoritative when present — it overrides the relay
// and Blossom lists a device would otherwise discover from the owner's key
// (NIP-65 / kind 10063). Discovery itself is an engine concern; this package
// only loads the local override and locates state.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ca.punkscience.tendrils/internal/keys"
)

const (
	configFile     = "config.json"
	keyFile        = "key"
	indexFile      = "index.db"
	daemonAddrFile = "daemon.addr"
	updateFile     = "update.json"
	// envHome overrides the state directory, for tests and power users.
	envHome = "TENDRILS_HOME"
	// envNoUpdateCheck disables the background update check outright. A daemon
	// on a locked-down host must be able to never phone home, without editing a
	// config file it may not own.
	envNoUpdateCheck = "TENDRILS_NO_UPDATE_CHECK"
)

// Config is the local, user-editable configuration.
type Config struct {
	// SyncRoot is the absolute path of the folder this device syncs. Each device
	// may place it wherever it likes; identity is by relative path, not root.
	SyncRoot string `json:"sync_root"`
	// Relays lists Nostr relay URLs. Empty means "discover from the key".
	Relays []string `json:"relays,omitempty"`
	// BlossomServers lists Blossom server URLs for blob storage. Empty means
	// "discover from the key".
	BlossomServers []string `json:"blossom_servers,omitempty"`
	// UpdateCheck enables the background check for a newer release. A pointer so
	// that absent means "the default", and Save round-trips a config that never
	// set it instead of writing an opinion the owner did not express.
	UpdateCheck *bool `json:"update_check,omitempty"`
}

// Dir returns the Tendrils state directory, honouring $TENDRILS_HOME, else the
// OS user-config dir (e.g. %AppData%\tendrils, ~/.config/tendrils).
func Dir() (string, error) {
	if h := strings.TrimSpace(os.Getenv(envHome)); h != "" {
		return h, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: locate user config dir: %w", err)
	}
	return filepath.Join(base, "tendrils"), nil
}

// ensureDir creates the state directory with owner-only permissions.
func ensureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("config: create %s: %w", dir, err)
	}
	return dir, nil
}

// IndexPath returns the path to the index database.
func IndexPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, indexFile), nil
}

// DaemonAddrPath returns the path to the file where a running daemon records the
// loopback address of its status endpoint. Its absence means no daemon is
// running; a stale file (from a crash) is harmless because the CLI dial-probes
// the address before trusting it.
func DaemonAddrPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonAddrFile), nil
}

// UpdatePath returns the path to the update-check cache. It lives in the state
// directory like everything else, so $TENDRILS_HOME isolates it in tests and a
// second identity on one machine keeps its own.
func UpdatePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, updateFile), nil
}

// UpdateCheckEnabled reports whether this device may check for a newer release.
// Both opt-outs are consulted here, before anything schedules a network call:
// TENDRILS_NO_UPDATE_CHECK in the environment, and "update_check": false in
// config.json.
//
// A config file that cannot be read or parsed disables the check. That is the
// conservative direction: the question being asked is "may this device talk to
// the network on its own behalf", and the honest answer when the owner's stated
// preference is unreadable is no.
func UpdateCheckEnabled() bool {
	if v := strings.TrimSpace(os.Getenv(envNoUpdateCheck)); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return false
	}
	cfg, found, err := Load()
	if err != nil {
		return false
	}
	if !found || cfg.UpdateCheck == nil {
		return true
	}
	return *cfg.UpdateCheck
}

// Load reads the local config file. A missing file is not an error: it returns
// a zero Config and found=false so callers can fall back to discovery.
func Load() (cfg Config, found bool, err error) {
	dir, err := Dir()
	if err != nil {
		return Config{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if os.IsNotExist(err) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("config: read: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("config: parse: %w", err)
	}
	return cfg, true, nil
}

// Save writes the local config file (creating the state directory).
func Save(cfg Config) error {
	dir, err := ensureDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFile), data, 0o600); err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	return nil
}

// SaveKey persists the device secret key with owner-only permissions so the
// daemon can resume without the owner re-entering it. This is deliberately a
// plain 0600 file, not an OS keychain: simple enough to fully understand and
// back up, which is the whole point of the tool. The owner is responsible for
// protecting and backing up this file (see docs/OPEN-QUESTIONS.md Q8).
func SaveKey(id *keys.Identity) error {
	dir, err := ensureDir()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), []byte(id.SecretHex()+"\n"), 0o600); err != nil {
		return fmt.Errorf("config: write key: %w", err)
	}
	return nil
}

// LoadKey reads and parses the stored device key. found=false if none is stored.
func LoadKey() (id *keys.Identity, found bool, err error) {
	dir, err := Dir()
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, keyFile))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("config: read key: %w", err)
	}
	id, err = keys.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, false, fmt.Errorf("config: stored key invalid: %w", err)
	}
	return id, true, nil
}
