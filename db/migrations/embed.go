// Package migrations embeds the goose SQL migrations so the API binary and
// the tests can apply them without the goose CLI.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
