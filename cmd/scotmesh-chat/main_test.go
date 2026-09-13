package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveAndLoad(t *testing.T) {
	dir := t.TempDir()
	fileConfig := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(fileConfig, []byte("backbone = \"rns.example.net:4242\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope.toml")

	t.Run("an explicit flag path must exist", func(t *testing.T) {
		if _, err := resolveAndLoad(missing, "", missing); err == nil {
			t.Fatal("expected an error for a named path that doesn't exist")
		}
	})
	t.Run("an explicit env path must exist", func(t *testing.T) {
		if _, err := resolveAndLoad("", missing, missing); err == nil {
			t.Fatal("expected an error for a named path that doesn't exist")
		}
	})
	t.Run("a missing fallback path is not an error", func(t *testing.T) {
		t.Setenv("SCOTMESH_CHAT_BACKBONE", "rns.example.net:4242")
		cfg, err := resolveAndLoad("", "", missing)
		if err != nil {
			t.Fatalf("a container configured by environment alone should still start: %v", err)
		}
		if cfg.Backbone != "rns.example.net:4242" {
			t.Errorf("backbone = %q", cfg.Backbone)
		}
	})
	t.Run("the flag wins over the environment and the fallback", func(t *testing.T) {
		cfg, err := resolveAndLoad(fileConfig, missing, missing)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backbone != "rns.example.net:4242" {
			t.Errorf("backbone = %q", cfg.Backbone)
		}
	})
	t.Run("the environment wins over the fallback", func(t *testing.T) {
		cfg, err := resolveAndLoad("", fileConfig, missing)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backbone != "rns.example.net:4242" {
			t.Errorf("backbone = %q", cfg.Backbone)
		}
	})
	t.Run("the fallback is used last", func(t *testing.T) {
		cfg, err := resolveAndLoad("", "", fileConfig)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backbone != "rns.example.net:4242" {
			t.Errorf("backbone = %q", cfg.Backbone)
		}
	})
}

func TestRunHealthcheck(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("backbone = \"rns.example.net:4242\"\ndata_dir = \""+dir+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHealth := func(t *testing.T, updatedAt time.Time, connected bool) {
		t.Helper()
		body := `{"updated_at":"` + updatedAt.Format(time.RFC3339) + `","backbone_connected":` +
			map[bool]string{true: "true", false: "false"}[connected] + `}`
		if err := os.WriteFile(filepath.Join(dir, "health.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Remove(filepath.Join(dir, "health.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if code := runHealthcheck([]string{"--config", configPath}); code == 0 {
		t.Error("no health.json at all should fail")
	}

	writeHealth(t, time.Now(), true)
	if code := runHealthcheck([]string{"--config", configPath}); code != 0 {
		t.Error("a fresh, connected health.json should pass")
	}

	writeHealth(t, time.Now(), false)
	if code := runHealthcheck([]string{"--config", configPath}); code == 0 {
		t.Error("backbone_connected: false should fail")
	}

	writeHealth(t, time.Now().Add(-time.Hour), true)
	if code := runHealthcheck([]string{"--config", configPath}); code == 0 {
		t.Error("a stale health.json should fail")
	}
	if code := runHealthcheck([]string{"--config", configPath, "--max-age", "2h"}); code != 0 {
		t.Error("--max-age should widen the staleness window")
	}
}
