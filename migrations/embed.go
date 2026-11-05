// Package migrations embeds the SQL migration files into the binary, so the
// migrate command needs no files on disk at runtime.
//
// This file lives next to the .sql files because go:embed cannot reference
// parent directories.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
