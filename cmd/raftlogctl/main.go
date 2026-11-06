// Command raftlogctl is the command-line client for raftlog.
package main

import "fmt"

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	// TODO(phase 1): subcommands: create-topic, produce, fetch, commit.
	fmt.Printf("raftlogctl %s\n", version)
}
