package dnslist

import _ "embed"

// ProvidersJSON holds the baseline repository DNS provider catalog embedded at build time.
//
//go:embed providers.json
var ProvidersJSON []byte
