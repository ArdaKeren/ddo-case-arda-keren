// Package migrations embeds the goose SQL migrations into the binary.
package migrations

import (
	"database/sql"
	"embed"

	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var FS embed.FS

// VersionTable is goose's bookkeeping table. It follows the qa- prefix and is
// pre-quoted because goose interpolates it into SQL verbatim and the name has a hyphen.
const VersionTable = `"qa-goose_db_version"`

// Up applies all pending migrations.
func Up(db *sql.DB) error {
	goose.SetBaseFS(FS)
	goose.SetTableName(VersionTable)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}
