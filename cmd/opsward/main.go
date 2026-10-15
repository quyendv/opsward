// Command opsward is a guarded gateway that lets AI agents operate infrastructure
// within the permissions an operator grants.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/quyendv/opsward/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "opsward: no commands yet")
	os.Exit(2)
}
