// Package docs guards documentation hygiene: every reference
// page links from the docs index, so a new page cannot land
// orphaned. The test reads the tree relative to this package
// (go test always runs with the package dir as cwd), which
// holds under the local and container suites alike.
package docs
