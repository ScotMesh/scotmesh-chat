// Package app wires the store, the hub core and the ways in onto one
// Reticulum transport, from a TOML config file.
package app

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ScotMesh/scotmesh-chat/internal/nomadpage"
)

// Config is the service configuration file.
type Config struct {
	DataDir  string   `toml:"data_dir"`
	Backbone string   `toml:"backbone"` // host:port of a Reticulum TCP server
	Admins   []string `toml:"admins"`   // identity hashes of hub operators
	LogLevel string   `toml:"log_level"`
	// LogFormat is "text" or "json". JSON suits a container, where logs go
	// to a collector rather than a human terminal.
	LogFormat string `toml:"log_format"`
	// Timezone is the IANA zone (e.g. "Europe/London") the nightly backup
	// runs on. The default, "UTC", always works with no host tzdata; main
	// imports time/tzdata so any zone works from a static binary or a
	// scratch container image too.
	//
	// Chat timestamps ("today" vs "Mon 15:04", LXMF delivery scheduling)
	// are a separate, longer-standing Europe/London default in internal/hub,
	// internal/lxmfgroup and internal/nomadpage; making that configurable
	// too is tracked as a follow-up, not done here.
	Timezone string `toml:"timezone"`

	Hub struct {
		Name            string       `toml:"name"`
		GroupRoom       string       `toml:"group_room"`
		Retention       Duration     `toml:"retention"`
		PostsPerMinute  int          `toml:"posts_per_minute"`
		MaxBodyBytes    int          `toml:"max_body_bytes"`
		ReplayMax       int          `toml:"replay_max"`
		ReplayFirstTime int          `toml:"replay_first_visit"`
		Rooms           []RoomConfig `toml:"rooms"`
	} `toml:"hub"`

	Group struct {
		Enabled          bool     `toml:"enabled"`
		Identity         string   `toml:"identity"` // file, relative to data_dir
		DisplayName      string   `toml:"display_name"`
		Name             string   `toml:"name"` // how replies refer to the group
		PropagationNode  string   `toml:"propagation_node"`
		AnnounceInterval Duration `toml:"announce_interval"`
		MaxStampCost     int      `toml:"max_stamp_cost"`
		JoinDigest       int      `toml:"join_digest"`
	} `toml:"group"`

	Page struct {
		Enabled          bool     `toml:"enabled"`
		Identity         string   `toml:"identity"` // file, relative to data_dir
		NodeName         string   `toml:"node_name"`
		Title            string   `toml:"title"`
		WikiURL          string   `toml:"wiki_url"` // a NomadNet page about the chat, "hash:/page/x.mu"
		AnnounceInterval Duration `toml:"announce_interval"`
		// BannerFile is a Micron file shown at the top of the chat page,
		// relative to data_dir. Unset selects the built-in default (the
		// Saltire); pointing it at an empty file (or /dev/null) means no
		// banner at all. See docs/banner.md.
		BannerFile string `toml:"banner_file"`
	} `toml:"page"`

	RRC struct {
		Enabled                 bool     `toml:"enabled"`
		Identity                string   `toml:"identity"` // file, relative to data_dir
		Greeting                []string `toml:"greeting"`
		AnnounceInterval        Duration `toml:"announce_interval"`
		IncludeJoinedMemberList bool     `toml:"include_joined_member_list"`
		MaxMsgBodyBytes         int      `toml:"max_msg_body_bytes"`
		RatePerMinute           int      `toml:"rate_per_minute"`
	} `toml:"rrc"`
}

// RoomConfig is a room the hub always keeps.
type RoomConfig struct {
	Name  string `toml:"name"`
	Topic string `toml:"topic"`
}

// Duration is a time.Duration written as "30m" in TOML.
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText writes a Go duration string.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Defaults is the configuration before a file or the environment override
// anything. Network identity — the backbone, the hub's name and room, a
// wiki link — is left unset here on purpose, so an operator must choose it;
// deploy/config.example.toml carries ScotMesh's own values.
func Defaults() Config {
	var c Config
	c.DataDir = "/var/lib/scotmesh-chat"
	c.LogLevel = "info"
	c.LogFormat = "text"
	c.Timezone = "UTC"
	c.Hub.Name = "Chat"
	c.Hub.GroupRoom = "chat"
	c.Hub.Retention = Duration{7 * 24 * time.Hour}
	c.Hub.Rooms = []RoomConfig{{Name: "chat"}}
	c.Group.Enabled = true
	c.Group.Identity = "identities/group"
	c.Group.DisplayName = "Chat – send /join"
	c.Group.Name = "Chat"
	c.Group.AnnounceInterval = Duration{30 * time.Minute}
	c.Group.MaxStampCost = 16
	c.Page.Enabled = true
	c.Page.Identity = "identities/page"
	c.Page.NodeName = "Chat"
	c.Page.Title = "Chat"
	c.Page.AnnounceInterval = Duration{30 * time.Minute}
	c.RRC.Enabled = true
	c.RRC.Identity = "identities/rrc-hub"
	c.RRC.AnnounceInterval = Duration{30 * time.Minute}
	return c
}

// Load builds the configuration from Defaults, an optional TOML file, and
// SCOTMESH_CHAT_* environment variables, each overriding the one before it.
// path == "" skips the file, for a container configured purely through its
// environment (docs/configuration.md lists every variable). Unknown TOML
// keys and unknown SCOTMESH_CHAT_* variables are both errors, so a typo
// can't silently leave a setting at its default.
func Load(path string) (Config, error) {
	c := Defaults()
	if path != "" {
		md, err := toml.DecodeFile(path, &c)
		if err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for i, k := range undecoded {
				keys[i] = k.String()
			}
			return c, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
		}
	}
	if err := applyEnv(&c, os.Environ()); err != nil {
		return c, err
	}
	err := c.Validate()
	return c, err
}

// Validate checks values that decoding cannot.
func (c *Config) Validate() error {
	var errs []error
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir is required"))
	}
	if c.Backbone == "" {
		errs = append(errs, errors.New("backbone is required"))
	} else if _, _, err := net.SplitHostPort(c.Backbone); err != nil {
		errs = append(errs, fmt.Errorf("backbone %q: %w", c.Backbone, err))
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("timezone %q: %w", c.Timezone, err))
	}
	if _, err := c.BannerLines(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.AdminHashes(); err != nil {
		errs = append(errs, err)
	}
	if c.Group.PropagationNode != "" {
		if b, err := hex.DecodeString(c.Group.PropagationNode); err != nil || len(b) != 16 {
			errs = append(errs, fmt.Errorf("group.propagation_node %q is not a 32-character address", c.Group.PropagationNode))
		}
	}
	if c.Group.AnnounceInterval.Duration < 10*time.Minute {
		errs = append(errs, errors.New("group.announce_interval must be at least 10m"))
	}
	if c.Page.AnnounceInterval.Duration < 10*time.Minute {
		errs = append(errs, errors.New("page.announce_interval must be at least 10m"))
	}
	if c.RRC.AnnounceInterval.Duration < 10*time.Minute {
		errs = append(errs, errors.New("rrc.announce_interval must be at least 10m: announces are costly on radio"))
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level %q: use debug, info, warn or error", c.LogLevel))
	}
	return errors.Join(errs...)
}

// AdminHashes decodes the admin identity hashes.
func (c *Config) AdminHashes() ([][]byte, error) {
	out := make([][]byte, 0, len(c.Admins))
	for _, a := range c.Admins {
		b, err := hex.DecodeString(strings.TrimSpace(a))
		if err != nil || len(b) != 16 {
			return nil, fmt.Errorf("admins: %q is not a 32-character identity hash", a)
		}
		out = append(out, b)
	}
	return out, nil
}

// BannerLines reads and checks page.banner_file, for the page's Config.
// BannerFile == "" returns (nil, nil): nomadpage.New then uses the built-in
// default (the Saltire). Otherwise it returns nomadpage.ParseBanner's
// result, which is never nil: a zero-length, non-nil slice means the
// operator asked for no banner at all (an empty file, or /dev/null).
func (c *Config) BannerLines() ([]string, error) {
	if c.Page.BannerFile == "" {
		return nil, nil
	}
	path := c.path(c.Page.BannerFile)
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		return nil, fmt.Errorf("page.banner_file %s: %w", path, err)
	}
	lines, err := nomadpage.ParseBanner(raw)
	if err != nil {
		return nil, fmt.Errorf("page.banner_file %s: %w", path, err)
	}
	return lines, nil
}

func (c *Config) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.DataDir, p)
}

func ensurePrivateDir(dir string) error {
	return os.MkdirAll(dir, 0o700) //nolint:gosec // operator-configured path
}
