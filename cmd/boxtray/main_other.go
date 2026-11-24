//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "boxtray runs on the Windows gaming PC only")
	os.Exit(1)
}
