package app

import (
	"context"
	"reflect"
	"testing"

	kritui_db "seesharpsi/kritui/db"
)

func TestMigrateGitHostsFromVersion35(t *testing.T) {
	database := openTestDatabase(t)
	if _, err := database.Exec(`DROP TABLE git_hosts`); err != nil {
		t.Fatalf("drop git_hosts: %v", err)
	}
	if _, err := database.Exec(`ALTER TABLE settings DROP COLUMN git_hosts_configured`); err != nil {
		t.Fatalf("drop git_hosts_configured: %v", err)
	}
	if _, err := database.Exec(`PRAGMA user_version = 35`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrateDatabase() error: %v", err)
	}

	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("user_version = %d, want %d", version, len(databaseMigrations))
	}
	var tableCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'git_hosts'`).Scan(&tableCount); err != nil {
		t.Fatalf("count git_hosts table: %v", err)
	}
	if tableCount != 1 {
		t.Fatalf("git_hosts table count = %d, want 1", tableCount)
	}
	hosts, err := kritui_db.GetGitHosts(context.Background(), database)
	if err != nil {
		t.Fatalf("GetGitHosts() error: %v", err)
	}
	if !reflect.DeepEqual(hosts, kritui_db.DefaultGitHosts()) {
		t.Fatalf("GetGitHosts() = %#v, want defaults", hosts)
	}
}
