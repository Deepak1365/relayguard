// migrate applies or rolls back the database schema.
// Usage: go run ./cmd/migrate [up|down|status]
package main

import (
	"database/sql"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/migrations"
)

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatalf("set dialect: %v", err)
	}

	switch command {
	case "up":
		err = goose.Up(db, ".")
	case "down":
		err = goose.Down(db, ".")
	case "status":
		err = goose.Status(db, ".")
	default:
		log.Fatalf("unknown command %q (use up, down, or status)", command)
	}
	if err != nil {
		log.Fatalf("migrate %s: %v", command, err)
	}
}
