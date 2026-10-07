// Package webfiles embeds the static files: CSS, icons, and the pinned
// Starbase components with their Datastar build.
package webfiles

import "embed"

//go:embed static
var FS embed.FS
