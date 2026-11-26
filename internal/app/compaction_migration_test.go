package app

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	kritui_db "seesharpsi/kritui/db"
)

func TestMigrateDatabaseAddsCompactionModelFromVersion40(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	column := "    compaction_model TEXT CHECK (compaction_model IS NULL OR length(CAST(compaction_model AS BLOB)) <= 512),\n"
	if !strings.Contains(schema, column) {
		t.Fatal("schema does not contain compaction_model column")
	}
	previous := strings.Replace(schema, column, "", 1)
	previous = strings.Replace(previous, "PRAGMA user_version = 42;", "PRAGMA user_version = 40;", 1)
	if !strings.Contains(previous, "PRAGMA user_version = 40;") {
		t.Fatal("failed to build version 40 fixture")
	}
	if _, err := database.Exec(previous); err != nil {
		t.Fatalf("initialize version 40 database: %v", err)
	}
	if err := migrateDatabase(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("migrated schema version = %d, want %d", version, len(databaseMigrations))
	}
	config, err := kritui_db.GetCompactionConfig(context.Background(), database)
	if err != nil {
		t.Fatalf("GetCompactionConfig() after migration error: %v", err)
	}
	if !config.Auto || config.Model != "" {
		t.Fatalf("migrated compaction config = %#v, want defaults", config)
	}
}
