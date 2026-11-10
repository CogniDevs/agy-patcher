package locales

import "embed"

// FS holds embedded localization JSON files.
//
//go:embed *.json
var FS embed.FS
