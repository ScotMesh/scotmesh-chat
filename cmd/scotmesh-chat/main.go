// scotmesh-chat is ScotMesh's chat hub: one conversation reached as an RRC
// room, an LXMF group and a NomadNet page.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // so any [timezone] setting works from a static binary or a scratch container image, without the host's zoneinfo

	"github.com/ScotMesh/scotmesh-chat/internal/app"
	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/importer"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// configEnvVar overrides --config's default when the flag is left at "",
// letting a container name its config file without a command-line change.
const configEnvVar = "SCOTMESH_CHAT_CONFIG"

// defaultConfigPath is tried when neither --config nor SCOTMESH_CHAT_CONFIG
// names a file. Unlike an explicitly named path, its absence is not an
// error: a container configured purely through the environment has no file
// here at all.
const defaultConfigPath = "/etc/scotmesh-chat/config.toml"

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run())
}

// run is main with an exit code, so deferred cleanup runs before the process exits.
func run() int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "import":
			return runImport(os.Args[2:])
		case "check-db":
			return runCheckDB(os.Args[2:])
		case "help-wiki":
			fmt.Print(hub.HelpWiki())
			return 0
		case "healthcheck":
			return runHealthcheck(os.Args[2:])
		}
	}
	configPath := flag.String("config", "", "config file (default: "+defaultConfigPath+" if present, or $"+configEnvVar+")")
	check := flag.Bool("check-config", false, "validate the config file and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("scotmesh-chat", version)
		return 0
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *check {
		fmt.Println("config ok")
		return 0
	}

	logHandler := slogHandler(cfg.LogFormat, os.Stderr, level(cfg.LogLevel))
	log := slog.New(logHandler)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, cfg, version, log); err != nil {
		log.Error("scotmesh-chat stopped", "err", err)
		return 1
	}
	return 0
}

// runImport brings rrc-hub's and bridge 0.1's state into the hub's store.
// The hub must not be running.
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file for data_dir and retention (default: "+defaultConfigPath+" if present, or $"+configEnvVar+")")
	rrcHub := fs.String("rrc-hub", "", "rrc-hub's data directory")
	bridge := fs.String("bridge", "", "bridge 0.1's data directory")
	skip := fs.String("skip", "", "comma-separated identity hashes whose names and messages are not imported (the bridge's RRC bot)")
	dryRun := fs.Bool("dry-run", false, "report what would be imported without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *rrcHub == "" && *bridge == "" {
		fmt.Fprintln(os.Stderr, "import: give --rrc-hub, --bridge or both")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	src := importer.Sources{RRCHub: *rrcHub, Bridge: *bridge}
	for _, h := range strings.Split(*skip, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 16 {
			fmt.Fprintf(os.Stderr, "import: --skip %q is not an identity hash\n", h)
			return 2
		}
		src.Skip = append(src.Skip, b)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "hub.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = st.Close() }()
	rep, err := importer.Import(ctx, st, src, time.Now(), cfg.Hub.Retention.Duration, *dryRun)
	if rep != nil {
		fmt.Print(rep)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "import failed; nothing was written:", err)
		return 1
	}
	if *dryRun {
		fmt.Println("dry run: nothing was written")
	}
	return 0
}

// runCheckDB opens a database, which brings its schema up to date, and
// reports what's in it: the dry run of an upgrade, on a copy of the live
// database.
func runCheckDB(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: scotmesh-chat check-db COPY_OF_HUB_DB")
		return 2
	}
	ctx := context.Background()
	path := filepath.Clean(args[0])
	before := "a new database"
	if _, err := os.Stat(path); err == nil { //nolint:gosec // the operator names the copy to check
		before = "an existing database"
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-db:", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	sum, err := st.Summary(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-db:", err)
		return 1
	}
	keys := make([]string, 0, len(sum))
	for k := range sum {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("opened %s; schema now version %d\n", before, sum["schema_version"])
	for _, k := range keys {
		if k != "schema_version" {
			fmt.Printf("  %-20s %d\n", k, sum[k])
		}
	}
	return 0
}

// loadConfig resolves the config file (flag, then $SCOTMESH_CHAT_CONFIG,
// then defaultConfigPath) and loads it with app.Load, which folds in
// SCOTMESH_CHAT_* environment variables regardless.
//
// A path named explicitly (by the flag or the environment variable) must
// exist: refusing to start is safer than silently running on defaults,
// which for years meant quietly joining ScotMesh's own network. Only
// defaultConfigPath, tried because nothing else was named, is allowed to be
// absent — that's the normal case for a container configured purely
// through its environment.
func loadConfig(flagPath string) (app.Config, error) {
	return resolveAndLoad(flagPath, os.Getenv(configEnvVar), defaultConfigPath)
}

// resolveAndLoad is loadConfig with the environment variable and the
// fallback path passed in, so tests don't depend on $SCOTMESH_CHAT_CONFIG
// or /etc/scotmesh-chat/config.toml on the machine running them.
func resolveAndLoad(flagPath, envPath, fallback string) (app.Config, error) {
	path, explicit := flagPath, flagPath != ""
	if path == "" {
		path, explicit = envPath, envPath != ""
	}
	if path == "" {
		path = fallback
	}
	if _, err := os.Stat(path); err != nil { //nolint:gosec // the operator names the config path (flag, env, or the fixed default)
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return app.Load("") // nothing named a file; build from defaults and the environment
		}
		return app.Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return app.Load(path)
}

// slogHandler builds the log handler named by format ("text" or "json").
func slogHandler(format string, w io.Writer, lvl slog.Level) slog.Handler {
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(format, "json") {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// runHealthcheck reports whether the running hub is healthy, from
// data_dir/health.json (written every 30s): recently updated, and
// connected to the backbone. Meant as a container's HEALTHCHECK command, or
// for an operator's own script; the running process isn't touched.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file (for data_dir)")
	maxAge := fs.Duration("max-age", 90*time.Second, "how stale health.json may be before this fails")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 2
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "health.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	var h struct {
		UpdatedAt         time.Time `json:"updated_at"`
		BackboneConnected bool      `json:"backbone_connected"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: health.json:", err)
		return 1
	}
	if age := time.Since(h.UpdatedAt); age > *maxAge {
		fmt.Fprintf(os.Stderr, "healthcheck: health.json is %s old (over %s)\n", age.Round(time.Second), *maxAge)
		return 1
	}
	if !h.BackboneConnected {
		fmt.Fprintln(os.Stderr, "healthcheck: not connected to the backbone")
		return 1
	}
	fmt.Println("healthy")
	return 0
}

func level(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
