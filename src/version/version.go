package version

// GitVersion is replaced at build time by the Makefile using -ldflags or by
// regenerating this file via the `src/version/version.go` make target.
var GitVersion = "dev"
