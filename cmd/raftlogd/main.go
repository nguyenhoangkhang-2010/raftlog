// Command raftlogd is the raftlog broker server.
package main

import "fmt"

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	// TODO(phase 1): parse flags, load config, start storage + gRPC server.
	fmt.Printf("raftlogd %s\n", version)
}
