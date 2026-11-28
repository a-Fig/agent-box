// Package cfworker embeds the Cloudflare fleet controller's deployable
// source, so box setup cloudflare can deploy it without a checkout of this
// repository.
package cfworker

import "embed"

// FS holds what wrangler deploy needs. Tests stay out.
//
//go:embed wrangler.jsonc package.json src/*.js
var FS embed.FS
