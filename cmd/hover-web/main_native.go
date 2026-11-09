//go:build !(js && wasm)

package main

// Native entry point for debugging the playground's front end without a
// browser: prints the sim.cpp the page would receive for a file on disk.
//
//	go run ./cmd/hover-web examples/Diode/rectifier.hvr
//
// The standard library is read from ./stdlib, as the playground bundles it.

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	args := os.Args[1:]
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hover-web <file.hvr>")
		os.Exit(2)
	}
	entry, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stdlib, _ := filepath.Abs("stdlib")
	res := compile(nil, entry, nativeStdlibRoots(stdlib))
	for _, l := range res.Log {
		fmt.Fprintln(os.Stderr, l)
	}
	if !res.OK {
		os.Exit(1)
	}
	fmt.Print(res.Code)
}

// nativeStdlibRoots is stdlibRoots for a directory on disk.
func nativeStdlibRoots(dir string) map[string]string {
	return stdlibRoots(os.DirFS(dir), dir)
}
