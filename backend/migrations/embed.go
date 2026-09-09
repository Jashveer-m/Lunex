// Package migrations embeds the SQL migration files into the binary so the api
// and migrate commands can run them without the golang-migrate CLI installed.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
