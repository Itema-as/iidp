// Command renderfixture renders one of iidp's built-in Create templates into
// a directory, so CI can docker build and run it without the full CLI. It is
// not part of the released iidp binary.
package main

import (
	"fmt"
	"os"

	"github.com/Itema-as/iidp/internal/templates"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: renderfixture <framework> <name> <dir>")
		os.Exit(2)
	}
	framework, name, dir := templates.Framework(os.Args[1]), os.Args[2], os.Args[3]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	files, err := templates.Render(framework, templates.Data{Name: name}, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, f := range files {
		fmt.Println(f)
	}
}
