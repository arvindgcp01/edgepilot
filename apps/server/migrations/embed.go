// Package migrations embeds the SQL schema into the binary.
//
// Embedding rather than shipping a directory means `epctl migrate up` cannot
// apply a schema that disagrees with the code in the same binary. A container
// image, a release archive, and a `go run` all carry exactly one schema, and it
// is the one they were built with.
package migrations

import "embed"

// FS holds every migration, named {version}_{title}.{up|down}.sql.
//
// Versions are dense and monotonic. A migration that has been merged is never
// edited: correcting a released migration means adding the next one, because
// anything already applied to a production database cannot be un-applied by
// changing the file it came from.
//
//go:embed *.sql
var FS embed.FS
