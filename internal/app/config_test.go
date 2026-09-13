package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestConfigurationDocIsComplete keeps docs/configuration.md from drifting:
// every SCOTMESH_CHAT_* variable applyEnv recognises must be named in the
// doc, so a new config field can't go undocumented.
func TestConfigurationDocIsComplete(t *testing.T) {
	fields := map[string]reflect.Value{}
	var c Config
	collectFields(reflect.ValueOf(&c).Elem(), "", fields)

	doc, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	var missing []string
	for name := range fields {
		if !strings.Contains(text, envPrefix+name) {
			missing = append(missing, envPrefix+name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/configuration.md doesn't mention: %v", missing)
	}
}

// TestLoadWithNoFile is the pure-environment case a container runs in: no
// TOML file at all, everything from SCOTMESH_CHAT_*.
func TestLoadWithNoFile(t *testing.T) {
	for k, v := range map[string]string{
		"SCOTMESH_CHAT_BACKBONE":              "rns.example.net:4242",
		"SCOTMESH_CHAT_DATA_DIR":              "/data",
		"SCOTMESH_CHAT_HUB_NAME":              "Highland Mesh",
		"SCOTMESH_CHAT_ADMINS":                "178c1f390b8f332f9c8681515c12107b,0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f",
		"SCOTMESH_CHAT_GROUP_ENABLED":         "false",
		"SCOTMESH_CHAT_RRC_GREETING":          `["hello there", "welcome"]`,
		"SCOTMESH_CHAT_HUB_ROOMS":             `[{"name":"lounge","topic":"General chat"}]`,
		"SCOTMESH_CHAT_HUB_RETENTION":         "48h",
		"SCOTMESH_CHAT_PAGE_BANNER_FILE":      "",
		"SCOTMESH_CHAT_RRC_ANNOUNCE_INTERVAL": "15m",
	} {
		t.Setenv(k, v)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backbone != "rns.example.net:4242" || cfg.DataDir != "/data" || cfg.Hub.Name != "Highland Mesh" {
		t.Errorf("scalar overlay: %+v", cfg)
	}
	if cfg.Group.Enabled {
		t.Error("bool overlay didn't apply")
	}
	if len(cfg.Admins) != 2 || cfg.Admins[1] != "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f" {
		t.Errorf("comma-separated list: %v", cfg.Admins)
	}
	if len(cfg.RRC.Greeting) != 2 || cfg.RRC.Greeting[0] != "hello there" {
		t.Errorf("JSON list: %v", cfg.RRC.Greeting)
	}
	if len(cfg.Hub.Rooms) != 1 || cfg.Hub.Rooms[0].Name != "lounge" || cfg.Hub.Rooms[0].Topic != "General chat" {
		t.Errorf("JSON object list: %+v", cfg.Hub.Rooms)
	}
	if cfg.Hub.Retention.Duration != 48*time.Hour {
		t.Errorf("duration overlay: %v", cfg.Hub.Retention.Duration)
	}
	if cfg.RRC.AnnounceInterval.Duration != 15*time.Minute {
		t.Errorf("nested duration overlay: %v", cfg.RRC.AnnounceInterval.Duration)
	}
}

// TestLoadEnvOverridesFile checks the override order: defaults, then the
// file, then the environment.
func TestLoadEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("backbone = \"rns.file.example:4242\"\n[hub]\nname = \"From file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOTMESH_CHAT_HUB_NAME", "From env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backbone != "rns.file.example:4242" {
		t.Errorf("file value lost: %q", cfg.Backbone)
	}
	if cfg.Hub.Name != "From env" {
		t.Errorf("env didn't override the file: %q", cfg.Hub.Name)
	}
}

func TestUnknownEnvVarIsAnError(t *testing.T) {
	t.Setenv("SCOTMESH_CHAT_BACKBONE", "rns.example.net:4242")
	t.Setenv("SCOTMESH_CHAT_HUB_NAEM", "typo")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an unknown SCOTMESH_CHAT_ variable")
	}
}

func TestBackboneMustBeHostPort(t *testing.T) {
	t.Setenv("SCOTMESH_CHAT_BACKBONE", "rns.example.net")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for a backbone with no port")
	}
}

func TestBadTimezoneIsAnError(t *testing.T) {
	t.Setenv("SCOTMESH_CHAT_BACKBONE", "rns.example.net:4242")
	t.Setenv("SCOTMESH_CHAT_TIMEZONE", "Nowhere/Imaginary")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an invalid timezone")
	}
}

func TestBannerFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := func() Config {
		c := Defaults()
		c.Backbone = "rns.example.net:4242"
		return c
	}

	c := base()
	if lines, err := c.BannerLines(); err != nil || lines != nil {
		t.Errorf("unset banner_file: %v %v, want (nil, nil)", lines, err)
	}

	c = base()
	c.Page.BannerFile = write("empty.mu", "")
	lines, err := c.BannerLines()
	if err != nil || lines == nil || len(lines) != 0 {
		t.Errorf("empty banner file: %v %v, want ([]string{}, nil)", lines, err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("an empty banner file should be valid: %v", err)
	}

	c = base()
	c.Page.BannerFile = write("custom.mu", "`B234`Fdef hello `f`b")
	lines, err = c.BannerLines()
	if err != nil || len(lines) != 1 {
		t.Errorf("custom banner file: %v %v", lines, err)
	}

	c = base()
	c.Page.BannerFile = write("directive.mu", "#!c=1\nhello")
	if _, err := c.BannerLines(); err == nil {
		t.Error("a page directive in the banner file should be refused")
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate should catch a bad banner file too")
	}

	c = base()
	c.Page.BannerFile = "does-not-exist.mu"
	if _, err := c.BannerLines(); err == nil {
		t.Error("a missing banner file should be refused")
	}
}
