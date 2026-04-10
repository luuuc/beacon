package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Retention RetentionConfig `yaml:"retention"`
	Rollup    RollupConfig    `yaml:"rollup"`
	Baseline  BaselineConfig  `yaml:"baseline"`
	Ingest    IngestConfig    `yaml:"ingest"`
}

type IngestConfig struct {
	// TrustXFF controls whether X-Forwarded-For is honored for the per-IP
	// rate limiter. Default false: when Beacon sits on a private network
	// behind a proxy you don't control, a forged XFF could bypass the
	// limiter. Set true only when the proxy rewrites XFF and your network
	// prevents direct reach to :4680.
	TrustXFF bool `yaml:"trust_xff"`
}

type ServerConfig struct {
	Bind         string     `yaml:"bind"`
	HTTPPort     int        `yaml:"http_port"`
	MCPPort      int        `yaml:"mcp_port"`
	PprofEnabled bool       `yaml:"pprof_enabled"`
	Auth         AuthConfig `yaml:"auth"`
}

type AuthConfig struct {
	Token string `yaml:"token"`
}

type DatabaseConfig struct {
	URL     string `yaml:"url"`
	Adapter string `yaml:"adapter"`
	Path    string `yaml:"path"`
	Schema  string `yaml:"schema"`
}

type RetentionConfig struct {
	EventsDays      int `yaml:"events_days"`
	RollupsHourDays int `yaml:"rollups_hour_days"`
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
			MCPPort:  4681,
		},
		Retention: RetentionConfig{
			EventsDays:      14,
			RollupsHourDays: 90,
		},
		Rollup: RollupConfig{
			TickSeconds: 60,
			PruneAt:     "03:00",
			Timezone:    "UTC",
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
	if v := os.Getenv("BEACON_DATABASE_SCHEMA"); v != "" {
		cfg.Database.Schema = v
	}
	if v := os.Getenv("BEACON_INGEST_TRUST_XFF"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("BEACON_INGEST_TRUST_XFF: %w", err)
		}
		cfg.Ingest.TrustXFF = b
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
	if src.Retention.EventsDays != 0 {
		dst.Retention.EventsDays = src.Retention.EventsDays
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
	return nil
}
