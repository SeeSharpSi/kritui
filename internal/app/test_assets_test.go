package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// projectRoot is the absolute repository root, used by tests that read legacy
// fixtures outside this package.
var projectRoot string

func TestMain(m *testing.M) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve project root: %v\n", err)
		os.Exit(1)
	}
	projectRoot = root

	assets := fstest.MapFS{}
	assetsRoot := os.DirFS(root)
	if err := fs.WalkDir(assetsRoot, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(assetsRoot, path)
		if err != nil {
			return err
		}
		assets[path] = &fstest.MapFile{
			Data: data,
			Mode: 0o444,
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "load static assets: %v\n", err)
		os.Exit(1)
	}
	staticFiles = assets

	schemaBytes, err := os.ReadFile(filepath.Join(root, "db", "schema.sql"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read database schema: %v\n", err)
		os.Exit(1)
	}
	schema = string(schemaBytes)
	os.Exit(m.Run())
}

func TestStaticFilesScopeMatchesEmbeddedAssets(t *testing.T) {
	if _, err := fs.ReadFile(staticFiles, "static/styles.css"); err != nil {
		t.Fatalf("read static asset: %v", err)
	}
	for _, path := range []string{"db/schema.sql", "main.go"} {
		if _, err := fs.ReadFile(staticFiles, path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("read %s = %v, want fs.ErrNotExist", path, err)
		}
	}
}
