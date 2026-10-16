// Package buildinfo carries the version stamped at link time.
package buildinfo

// Version is set with
// -ldflags "-X github.com/Ricardoleaom/go-high-load-wallet/internal/buildinfo.Version=<version>";
// go run and tests keep "dev".
var Version = "dev"
