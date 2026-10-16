// Package web contains the public UI shipped in the server executable.
package web

import "embed"

// Assets contains only public templates and static assets, never runtime keys.
//
//go:embed templates static
var Assets embed.FS
