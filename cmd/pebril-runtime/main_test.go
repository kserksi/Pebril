package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func capture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	previous := os.Stdout
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	os.Stdout = file
	defer func() { os.Stdout = previous }()
	err = run(context.Background(), args)
	if _, e := file.Seek(0, 0); e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(file)
	if e != nil {
		t.Fatal(e)
	}
	return string(body), err
}

func TestSettingsUsesDataOnlyAndNULProtocol(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PEBRIL_NATIVE_DIR", "")
	if err := os.Unsetenv("PEBRIL_NATIVE_DIR"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PEBRIL_AVIF", "true")
	marker := filepath.Join(root, "injected")
	configured := "bin $(touch " + marker + ")"
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("PEBRIL_LISTEN=127.0.0.1:8181\nPEBRIL_NATIVE_DIR='"+configured+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := checkStartup
	checkStartup = func(allow, checkOnly bool) (bool, error) { return false, nil }
	t.Cleanup(func() { checkStartup = previous })
	body, err := capture(t, "settings", root)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(body, "\x00")
	if len(fields) != 5 || fields[0] != filepath.Join(root, configured) || fields[1] != "true" || fields[2] != root || fields[3] != "false" || fields[4] != "" {
		t.Fatal("Invalid settings protocol", fields)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Shell data was executed")
	}
}

func TestMalformedCommandsFailWithoutProtocolOutput(t *testing.T) {
	native := t.TempDir()
	for _, args := range [][]string{
		{}, {"unknown"}, {"host", "extra"}, {"settings"}, {"settings", native, "extra"},
		{"probe", "missing", native}, {"find", native, "invalid"}, {"install", "arbitrary-url", native, "false"},
	} {
		body, err := capture(t, args...)
		if err == nil || body != "" {
			t.Fatalf("Malformed command accepted or wrote protocol data: %q %q %v", args, body, err)
		}
	}
	for _, path := range []string{"line\nbreak", "line\rbreak", "nul\x00byte"} {
		if safePath(path) == nil {
			t.Fatal("Unsafe protocol path accepted")
		}
	}
}

func TestRestrictedSettingsDoesNotPersistOrConflateGrants(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PEBRIL_AVIF", "true")
	previous := checkStartup
	t.Cleanup(func() { checkStartup = previous })
	checkStartup = func(allow, checkOnly bool) (bool, error) {
		if !allow && !checkOnly {
			return false, errors.New("explicit startup consent required")
		}
		return true, nil
	}
	for _, options := range [][]string{{"--allow-low-memory"}, {"--check"}} {
		body, err := capture(t, append([]string{"settings", root}, options...)...)
		fields := strings.Split(body, "\x00")
		if err != nil || len(fields) != 5 || fields[1] != "false" || fields[3] != "true" {
			t.Fatalf("Invalid restricted settings: %q %v", body, err)
		}
	}
	for _, options := range [][]string{{}, {"--allow-downloads"}, {"--allow-system-install"}} {
		body, err := capture(t, append([]string{"settings", root}, options...)...)
		if err == nil || body != "" {
			t.Fatalf("Unrelated/persisted consent accepted: %q %v", body, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); !os.IsNotExist(err) {
		t.Fatal("Settings initialized business configuration", err)
	}
}
