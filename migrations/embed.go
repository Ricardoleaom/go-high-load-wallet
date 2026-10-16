// Package migrations embeds the SQL schema migrations, so tests apply
// exactly the files the migrate container applies.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
