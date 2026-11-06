package main

import (
	"errors"
	"strings"
)

var errNoAddress = errors.New("no broker address given")

// splitAddrs turns "a:1, b:2" into its addresses.
func splitAddrs(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, errNoAddress
	}
	return out, nil
}
