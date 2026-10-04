// Package kpr embeds the GPL text at module root (go:embed forbids
// parent references, so no subpackage can do it): the binary carries
// the license even where the checkout isn't around.
package kpr

import _ "embed"

//go:embed LICENSE
var text string

// Text is the full GPL text from LICENSE.
func Text() string { return text }
