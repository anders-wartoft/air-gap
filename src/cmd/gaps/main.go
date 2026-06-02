package main

import "sitia.nu/airgap/src/gaps"

// BuildNumber is set at build time via -ldflags
var BuildNumber = "dev"

func main() {
	gaps.Main(BuildNumber)
}
