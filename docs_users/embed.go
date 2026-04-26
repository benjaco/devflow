package docsusers

import "embed"

// Files is embedded by `devflow docs`.
//
//go:embed *.md
var Files embed.FS
