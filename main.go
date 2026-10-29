package main

import (
	"embed"
	"log"

	"seesharpsi/kritui/internal/app"
)

// staticFiles holds the browser assets served under /static/.
//
//go:embed static
var staticFiles embed.FS

// schema holds the SQL used to initialize a new database.
//
//go:embed db/schema.sql
var schema string

func main() {
	if err := app.Run(staticFiles, schema); err != nil {
		log.Fatal(err)
	}
}
