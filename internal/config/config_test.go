package config

import (
	"os"
	"path/filepath"
	"testing"

	"ca.punkscience.tendrils/internal/keys"
)

// isolate points TENDRILS_HOME at a temp dir for the duration of a test.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv(envHome, t.TempDir())
}

func TestLoadMissingReturnsNotFound(t *testing.T) {
	isolate(t)
	_, found, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Errorf("expected not found for absent config")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	isolate(t)
	in := Config{
		SyncRoot:       "/home/sam/vault",
		Relays:         []string{"wss://relay.example"},
		BlossomServers: []string{"https://blossom.example"},
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found after save")
	}
	if got.SyncRoot != in.SyncRoot || len(got.Relays) != 1 || got.Relays[0] != in.Relays[0] {
		t.Errorf("config round-trip mismatch: %+v", got)
	}
}

func TestKeyPersistsAndResumes(t *testing.T) {
	isolate(t)
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveKey(id); err != nil {
		t.Fatal(err)
	}
	// Simulates a reboot: a fresh LoadKey with no re-entry.
	got, found, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected stored key to be found")
	}
	if got.SecretHex() != id.SecretHex() {
		t.Errorf("resumed key differs from stored key")
	}
}

func TestLoadKeyMissingReturnsNotFound(t *testing.T) {
	isolate(t)
	_, found, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Errorf("expected no key before enrollment")
	}
}

func TestUpdatePathIsInTheStateDir(t *testing.T) {
	isolate(t)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := UpdatePath()
	if err != nil {
		t.Fatal(err)
	}
	// $TENDRILS_HOME has to isolate the update cache like everything else, or a
	// test — or a second identity on one machine — reads another's.
	if want := filepath.Join(dir, "update.json"); got != want {
		t.Errorf("UpdatePath() = %q, want %q", got, want)
	}
}

// With no opinion expressed anywhere, the check is on: a device that cannot
// tell it is stale is the problem the check exists to solve.
func TestUpdateCheckDefaultsOn(t *testing.T) {
	isolate(t)
	if !UpdateCheckEnabled() {
		t.Error("update check should default to enabled")
	}
	if err := Save(Config{SyncRoot: "/tmp/x"}); err != nil {
		t.Fatal(err)
	}
	if !UpdateCheckEnabled() {
		t.Error("a config that says nothing about update_check leaves it enabled")
	}
	// A config that never set it must not acquire an opinion by round-tripping.
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateCheck != nil {
		t.Errorf("update_check should stay absent, got %v", *cfg.UpdateCheck)
	}
}

// Both opt-outs are consulted before anything schedules a network call. A
// daemon on a locked-down host must be able to never phone home, whether the
// owner says so in the environment or in the config file.
func TestUpdateCheckOptOut(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		isolate(t)
		for _, v := range []string{"1", "true", "yes", "please-dont"} {
			t.Setenv(envNoUpdateCheck, v)
			if UpdateCheckEnabled() {
				t.Errorf("%s=%q should disable the check", envNoUpdateCheck, v)
			}
		}
		// The unset forms stay enabled, so an empty variable in a service file
		// does not silently turn the check off.
		for _, v := range []string{"", "0", "false"} {
			t.Setenv(envNoUpdateCheck, v)
			if !UpdateCheckEnabled() {
				t.Errorf("%s=%q should leave the check enabled", envNoUpdateCheck, v)
			}
		}
	})

	t.Run("config file", func(t *testing.T) {
		isolate(t)
		off := false
		if err := Save(Config{SyncRoot: "/tmp/x", UpdateCheck: &off}); err != nil {
			t.Fatal(err)
		}
		if UpdateCheckEnabled() {
			t.Error(`"update_check": false should disable the check`)
		}
		on := true
		if err := Save(Config{SyncRoot: "/tmp/x", UpdateCheck: &on}); err != nil {
			t.Fatal(err)
		}
		if !UpdateCheckEnabled() {
			t.Error(`"update_check": true should enable the check`)
		}
	})

	t.Run("unreadable config", func(t *testing.T) {
		isolate(t)
		dir, err := Dir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The question is "may this device talk to the network on its own
		// behalf". With the owner's stated preference unreadable, the honest
		// answer is no.
		if UpdateCheckEnabled() {
			t.Error("an unparsable config must not be read as consent")
		}
	})
}
