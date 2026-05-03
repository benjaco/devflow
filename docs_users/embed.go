package docsusers

import "embed"

// Files is embedded by `devflow docs setup` and `devflow docs development`.
//
//go:embed *.md
var Files embed.FS
