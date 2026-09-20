// Command iidp is the CLI developers run to create and change Applications
// on Itema's Platform.
package main

import (
	"os"

	"github.com/Itema-as/iidp/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
