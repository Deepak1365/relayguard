// Package migrations embeds the SQL schema files so they ship inside the binary.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
