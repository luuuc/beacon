package config

import (
	"errors"
	"fmt"
	"os"
	"path" // path (not path/filepath) — URL paths use forward slashes
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Retention RetentionConfig `yaml:"retention"`
	Rollup    RollupConfig    `yaml:"rollup"`
	Baseline  BaselineConfig  `yaml:"baseline"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Ambient   AmbientConfig   `yaml:"ambient"`
	Filter    FilterConfig    `yaml:"filter"`
}

type FilterConfig struct {
	ExcludePaths []string `yaml:"exclude_paths"`
	Defaults     *bool    `yaml:"defaults"` // nil = use defaults; false = disable built-in patterns
}

type AmbientConfig struct {
	Anomaly AnomalyConfig `yaml:"anomaly"`
}

type AnomalyConfig struct {
	BaselineWindow  string  `yaml:"baseline_window"`
	DetectionWindow string  `yaml:"detection_window"`
	SigmaThreshold  float64 `yaml:"sigma_threshold"`
	MinVolume       int     `yaml:"min_volume"`
}

type IngestConfig struct {
	// TrustXFF controls whether X-Forwarded-For is honored for the per-IP
	// rate limiter. Default false: when Beacon sits on a private network
	// behind a proxy you don't control, a forged XFF could bypass the
	// limiter. Set true only when the proxy rewrites XFF and your network
	// prevents direct reach to :4680.
	TrustXFF bool `yaml:"trust_xff"`

	// IdempMaxEntries bounds the in-memory idempotency ring. Zero lets
	// the handler pick its default (100k). Raise only when you know why.
	IdempMaxEntries int `yaml:"idemp_max_entries"`
}

type ServerConfig struct {
	Bind         string     `yaml:"bind"`
	HTTPPort     int        `yaml:"http_port"`
	MCPPort      int        `yaml:"mcp_port"`      // Deprecated: ignored. MCP is served on HTTPPort.
	MCPPortSet   bool       `yaml:"-"`              // True when mcp_port was explicitly set via file or env.
	PprofEnabled bool       `yaml:"pprof_enabled"`
	Auth         AuthConfig `yaml:"auth"`
}

type AuthConfig struct {
	Token string `yaml:"token"`
}

type DatabaseConfig struct {
	URL      string `yaml:"url"`
	Adapter  string `yaml:"adapter"`
	Path     string `yaml:"path"`
	Schema   string `yaml:"schema"`
	MaxConns int    `yaml:"max_conns"` // pgx pool cap; 0 = pgx default. Postgres only.
}

type RetentionConfig struct {
	EventsDays        int `yaml:"events_days"`
	AmbientRetentionHours int `yaml:"ambient_retention_hours"`
	RollupsHourDays   int `yaml:"rollups_hour_days"`
}

type RollupConfig struct {
	TickSeconds int    `yaml:"tick_seconds"`
	PruneAt     string `yaml:"prune_at"`
	Timezone    string `yaml:"timezone"`
}

type BaselineConfig struct {
	Windows []string `yaml:"windows"`
}

func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Bind:     "127.0.0.1",
			HTTPPort: 4680,
		},
		Retention: RetentionConfig{
			EventsDays:         14,
			AmbientRetentionHours: 24,
			RollupsHourDays:    90,
		},
		Rollup: RollupConfig{
			TickSeconds: 60,
			PruneAt:     "03:00",
			Timezone:    "UTC",
		},
		Ambient: AmbientConfig{
			Anomaly: AnomalyConfig{
				BaselineWindow:  "14d",
				DetectionWindow: "24h",
				SigmaThreshold:  2.0,
				MinVolume:       10,
			},
		},
		Baseline: BaselineConfig{
			Windows: []string{"24h", "7d", "30d"},
		},
	}
}

// Load reads beacon.yml at path (empty path = defaults only) and applies
// BEACON_* environment overrides. Env always wins over file; file wins over
// the zero value; any field left unset is populated from Defaults().
func Load(path string) (*Config, error) {
	cfg := Defaults()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var fromFile Config
		if err := yaml.Unmarshal(raw, &fromFile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		mergeNonZero(&cfg, &fromFile)
	}

	if err := applyEnv(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyEnv(cfg *Config) error {
	if v := os.Getenv("BEACON_BIND"); v != "" {
		cfg.Server.Bind = v
	}
	if v := os.Getenv("BEACON_HTTP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("BEACON_HTTP_PORT: %w", err)
		}
		cfg.Server.HTTPPort = n
	}
	if v := os.Getenv("BEACON_MCP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("BEACON_MCP_PORT: %w", err)
		}
		cfg.Server.MCPPort = n
		cfg.Server.MCPPortSet = true
	}
	if v := os.Getenv("BEACON_AUTH_TOKEN"); v != "" {
		cfg.Server.Auth.Token = v
	}
	if v := os.Getenv("BEACON_PPROF_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("BEACON_PPROF_ENABLED: %w", err)
		}
		cfg.Server.PprofEnabled = b
	}
	if v := os.Getenv("BEACON_DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("BEACON_DATABASE_ADAPTER"); v != "" {
		cfg.Database.Adapter = v
	}
	if v := os.Getenv("BEACON_DATABASE_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("BEACON_DATABASE_SCHEMA"); v != "" {
		cfg.Database.Schema = v
	}
	if v := os.Getenv("BEACON_DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("BEACON_DATABASE_MAX_CONNS: %w", err)
		}
		cfg.Database.MaxConns = n
	}
	if v := os.Getenv("BEACON_INGEST_TRUST_XFF"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("BEACON_INGEST_TRUST_XFF: %w", err)
		}
		cfg.Ingest.TrustXFF = b
	}
	if v := os.Getenv("BEACON_INGEST_IDEMP_MAX_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("BEACON_INGEST_IDEMP_MAX_ENTRIES: %w", err)
		}
		cfg.Ingest.IdempMaxEntries = n
	}
	if v := os.Getenv("BEACON_FILTER_EXCLUDE_PATHS"); v != "" {
		cfg.Filter.ExcludePaths = strings.Split(v, ",")
	}
	if v := os.Getenv("BEACON_FILTER_DEFAULTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("BEACON_FILTER_DEFAULTS: %w", err)
		}
		cfg.Filter.Defaults = &b
	}
	return nil
}

// mergeNonZero overlays src fields onto dst when the src field is non-zero.
// Keeps defaults for anything the YAML file leaves blank.
func mergeNonZero(dst, src *Config) {
	if src.Server.Bind != "" {
		dst.Server.Bind = src.Server.Bind
	}
	if src.Server.HTTPPort != 0 {
		dst.Server.HTTPPort = src.Server.HTTPPort
	}
	if src.Server.MCPPort != 0 {
		dst.Server.MCPPort = src.Server.MCPPort
		dst.Server.MCPPortSet = true
	}
	if src.Server.PprofEnabled {
		dst.Server.PprofEnabled = true
	}
	if src.Server.Auth.Token != "" {
		dst.Server.Auth.Token = src.Server.Auth.Token
	}
	if src.Database.URL != "" {
		dst.Database.URL = src.Database.URL
	}
	if src.Database.Adapter != "" {
		dst.Database.Adapter = src.Database.Adapter
	}
	if src.Database.Path != "" {
		dst.Database.Path = src.Database.Path
	}
	if src.Database.Schema != "" {
		dst.Database.Schema = src.Database.Schema
	}
	if src.Database.MaxConns != 0 {
		dst.Database.MaxConns = src.Database.MaxConns
	}
	if src.Retention.EventsDays != 0 {
		dst.Retention.EventsDays = src.Retention.EventsDays
	}
	if src.Retention.AmbientRetentionHours != 0 {
		dst.Retention.AmbientRetentionHours = src.Retention.AmbientRetentionHours
	}
	if src.Retention.RollupsHourDays != 0 {
		dst.Retention.RollupsHourDays = src.Retention.RollupsHourDays
	}
	if src.Rollup.TickSeconds != 0 {
		dst.Rollup.TickSeconds = src.Rollup.TickSeconds
	}
	if src.Rollup.PruneAt != "" {
		dst.Rollup.PruneAt = src.Rollup.PruneAt
	}
	if src.Rollup.Timezone != "" {
		dst.Rollup.Timezone = src.Rollup.Timezone
	}
	if len(src.Baseline.Windows) > 0 {
		dst.Baseline.Windows = src.Baseline.Windows
	}
	if src.Ingest.TrustXFF {
		dst.Ingest.TrustXFF = true
	}
	if src.Ingest.IdempMaxEntries != 0 {
		dst.Ingest.IdempMaxEntries = src.Ingest.IdempMaxEntries
	}
	if src.Ambient.Anomaly.BaselineWindow != "" {
		dst.Ambient.Anomaly.BaselineWindow = src.Ambient.Anomaly.BaselineWindow
	}
	if src.Ambient.Anomaly.DetectionWindow != "" {
		dst.Ambient.Anomaly.DetectionWindow = src.Ambient.Anomaly.DetectionWindow
	}
	if src.Ambient.Anomaly.SigmaThreshold != 0 {
		dst.Ambient.Anomaly.SigmaThreshold = src.Ambient.Anomaly.SigmaThreshold
	}
	if src.Ambient.Anomaly.MinVolume != 0 {
		dst.Ambient.Anomaly.MinVolume = src.Ambient.Anomaly.MinVolume
	}
	if len(src.Filter.ExcludePaths) > 0 {
		dst.Filter.ExcludePaths = src.Filter.ExcludePaths
	}
	if src.Filter.Defaults != nil {
		dst.Filter.Defaults = src.Filter.Defaults
	}
}

// IsLoopbackBind reports whether the bind address is restricted to loopback.
func (c *Config) IsLoopbackBind() bool {
	switch c.Server.Bind {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// Validate enforces invariants that must hold before the server starts.
// The loopback guard lives here: refuse non-loopback bind when auth.token is unset.
func (c *Config) Validate() error {
	if c.Server.HTTPPort <= 0 || c.Server.HTTPPort > 65535 {
		return fmt.Errorf("server.http_port out of range: %d", c.Server.HTTPPort)
	}
	if c.Server.Bind == "" {
		return errors.New("server.bind must be set")
	}
	if !c.IsLoopbackBind() && c.Server.Auth.Token == "" {
		return fmt.Errorf(
			"refusing to bind non-loopback interface %q with no auth.token set: "+
				"Beacon has no built-in auth; set server.auth.token or bind to 127.0.0.1",
			c.Server.Bind,
		)
	}
	if c.Database.MaxConns < 0 || c.Database.MaxConns > 200 {
		return fmt.Errorf(
			"database.max_conns out of range: %d (expected 0 for pgx default, or 1..200)",
			c.Database.MaxConns,
		)
	}
	if c.Ingest.IdempMaxEntries < 0 {
		return fmt.Errorf("ingest.idemp_max_entries must be ≥ 0 (got %d)", c.Ingest.IdempMaxEntries)
	}
	// pprof exposes heap, goroutine, and CPU profiles — it must never be
	// reachable from outside the host. The bearer token is not a sufficient
	// guard here: profiling endpoints take arbitrary durations and are a
	// natural DoS vector. Refuse the combination at boot instead of
	// papering over it with a middleware check.
	for _, pat := range c.Filter.ExcludePaths {
		if _, err := path.Match(pat, "/"); err != nil {
			return fmt.Errorf("filter.exclude_paths: invalid glob pattern %q: %w", pat, err)
		}
	}
	if c.Server.PprofEnabled && !c.IsLoopbackBind() {
		return fmt.Errorf(
			"refusing to enable pprof on non-loopback bind %q: "+
				"/debug/pprof exposes heap and CPU profiles and must only be reachable via 127.0.0.1",
			c.Server.Bind,
		)
	}
	return nil
}

// ParseBeaconDuration parses a duration string like "14d", "24h", "7d".
// The "d" suffix (days) extends Go's time.ParseDuration which handles
// "h", "m", "s", "ms", etc. natively. For any string not ending in "d",
// the standard library parser handles it — including compound forms like
// "1h30m".
func ParseBeaconDuration(s string) (time.Duration, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty duration")
	}
	if s[len(s)-1] == 'd' {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("parse %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// defaultExcludePaths is the built-in set of scanner/bot path patterns that
// Beacon drops at ingest. These cover WordPress vulnerability probes, PHP
// scanners, and CGI-bin enumeration — traffic that pollutes metrics on any
// internet-facing app. User-configured filter.exclude_paths extends this
// list; filter.defaults: false disables it.
var defaultExcludePaths = []string{
	"*.php",
	"/wp-*",
	"/cgi-bin*",
	"/xmlrpc*",
}

// DefaultExcludePaths returns a copy of the built-in scanner path patterns.
func DefaultExcludePaths() []string {
	out := make([]string, len(defaultExcludePaths))
	copy(out, defaultExcludePaths)
	return out
}

// PathFilter checks whether a request path should be excluded from ingest.
type PathFilter struct {
	patterns []string
}

// NewPathFilter builds a filter from the config. When cfg.Defaults is nil or
// true, built-in scanner patterns are included. User patterns are always added.
func NewPathFilter(cfg FilterConfig) *PathFilter {
	var patterns []string
	if cfg.Defaults == nil || *cfg.Defaults {
		patterns = append(patterns, defaultExcludePaths...)
	}
	patterns = append(patterns, cfg.ExcludePaths...)
	return &PathFilter{patterns: patterns}
}

// Patterns returns a copy of the active exclusion patterns (for startup logging).
func (f *PathFilter) Patterns() []string {
	out := make([]string, len(f.patterns))
	copy(out, f.patterns)
	return out
}

// ShouldExclude reports whether the given path matches any exclusion pattern.
// p should be the URL path portion (e.g. "/items/123"), not the full event
// name (e.g. "GET /items/123") — the caller must strip the HTTP method.
//
// It uses Go's path.Match semantics: * matches any non-separator sequence,
// ? matches a single non-separator character.
//
// Patterns starting with "/" are matched against the full path. Patterns
// without a leading "/" (e.g. "*.php") are matched against the last path
// segment (basename), so "*.php" catches "/wp-content/foo.php" as well as
// "/foo.php".
func (f *PathFilter) ShouldExclude(p string) bool {
	base := path.Base(p)
	for _, pat := range f.patterns {
		if pat != "" && pat[0] == '/' {
			if matched, _ := path.Match(pat, p); matched {
				return true
			}
		} else {
			if matched, _ := path.Match(pat, base); matched {
				return true
			}
		}
	}
	return false
}
