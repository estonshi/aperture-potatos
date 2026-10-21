// Command conform compares the outputs a binding's conformance runner wrote with the expected files, by the rules every binding shares (conformance/runner-protocol.md).
//
//	conform diff   <cases> <out>
//	conform update <cases> <out>
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/estonshi/aperture-potatos/conformance/compare"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[0] != "diff" && args[0] != "update" {
		fmt.Fprintln(stderr, "usage: conform diff|update <cases> <out>")
		fmt.Fprintln(stderr, "  <cases>  the conformance directory, a subtree of it, or one case")
		fmt.Fprintln(stderr, "  <out>    the directory the runner wrote the outputs of <cases> to")
		return 2
	}
	cases, err := compare.Cases(args[1], args[2])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(cases) == 0 {
		fmt.Fprintf(stderr, "no cases under %s\n", args[1])
		return 2
	}
	if args[0] == "update" {
		return update(cases, stdout, stderr)
	}
	n := map[compare.Status]int{}
	for _, c := range cases {
		r := compare.Check(c.Path, c.Out)
		n[r.Status]++
		switch {
		case r.Status == compare.Passed:
		case r.Detail == "":
			fmt.Fprintf(stdout, "%s\t%s\n", r.Status, c.ID)
		default:
			fmt.Fprintf(stdout, "%s\t%s: %s\n", r.Status, c.ID, r.Detail)
		}
	}
	fmt.Fprintf(stdout, "%d passed, %d failed, %d skipped, %d not run\n", n[compare.Passed], n[compare.Failed], n[compare.Skipped], n[compare.NotRun])
	if n[compare.Failed]+n[compare.NotRun] > 0 {
		return 1
	}
	return 0
}

func update(cases []compare.Case, stdout, stderr io.Writer) int {
	code, files := 0, 0
	wd, _ := os.Getwd()
	for _, c := range cases {
		if _, err := os.Stat(c.Out); err != nil {
			continue // not run, so there is nothing to accept
		}
		changed, err := compare.Update(c.Path, c.Out)
		for _, f := range changed {
			if r, err := filepath.Rel(wd, f); err == nil {
				f = r
			}
			fmt.Fprintln(stdout, "updated", f)
		}
		files += len(changed)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", c.ID, err)
			code = 1
		}
	}
	fmt.Fprintf(stdout, "%d files updated\n", files)
	return code
}
