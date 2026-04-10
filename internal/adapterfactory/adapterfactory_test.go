package adapterfactory

import (
	"strings"
	"testing"

	"github.com/luuuc/beacon/internal/config"
)

func TestResolveKind(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.DatabaseConfig
		want    string
		wantErr string
	}{
		{"explicit postgres", config.DatabaseConfig{Adapter: "postgres"}, "postgres", ""},
		{"explicit postgresql alias", config.DatabaseConfig{Adapter: "postgresql"}, "postgres", ""},
		{"explicit mysql", config.DatabaseConfig{Adapter: "mysql"}, "mysql", ""},
		{"explicit mariadb alias", config.DatabaseConfig{Adapter: "mariadb"}, "mysql", ""},
		{"explicit sqlite", config.DatabaseConfig{Adapter: "sqlite"}, "sqlite", ""},
		{"explicit sqlite3 alias", config.DatabaseConfig{Adapter: "sqlite3"}, "sqlite", ""},
		{"unknown adapter", config.DatabaseConfig{Adapter: "oracle"}, "", "unknown database.adapter"},
		{"infer postgres from url", config.DatabaseConfig{URL: "postgres://host/db"}, "postgres", ""},
		{"infer postgres from postgresql url", config.DatabaseConfig{URL: "postgresql://host/db"}, "postgres", ""},
		{"mysql url needs explicit adapter", config.DatabaseConfig{URL: "mysql://root@tcp(h)/db"}, "", "set database.adapter: mysql explicitly"},
		{"infer sqlite from path", config.DatabaseConfig{Path: "/var/lib/beacon/x.db"}, "sqlite", ""},
		{"empty cfg errors", config.DatabaseConfig{}, "", "database config is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveKind(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
