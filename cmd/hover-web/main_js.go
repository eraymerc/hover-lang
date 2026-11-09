//go:build js && wasm

package main

// Browser entry point. Exposes two functions on globalThis.hover:
//
//	hover.init(stdlibJSON)   // {"math/math.hvr": "...", ...} — once, at load
//	hover.compile(source)    // -> JSON compileResult {ok, code, log}
//
// The page's worker (website/assets/js/playground-worker.js) calls compile
// with the editor's buffer, then hands the returned sim.cpp to clang.
//
// Sources live in an in-memory filesystem: the bundled standard library
// under /stdlib, the buffer at /work/main.hvr. The loader reads both through
// loader.SetFS, so the pipeline is the same one the CLI runs.

import (
	"encoding/json"
	"syscall/js"
	"testing/fstest"
)

const (
	stdlibPrefix = "/stdlib"
	entryPath    = "/work/main.hvr"
)

var stdlib = fstest.MapFS{}

func main() {
	api := js.Global().Get("Object").New()
	api.Set("init", js.FuncOf(func(_ js.Value, args []js.Value) any {
		files := map[string]string{}
		if len(args) > 0 {
			if err := json.Unmarshal([]byte(args[0].String()), &files); err != nil {
				return "stdlib bundle: " + err.Error()
			}
		}
		stdlib = fstest.MapFS{}
		for path, src := range files {
			stdlib[path] = &fstest.MapFile{Data: []byte(src)}
		}
		return nil
	}))
	api.Set("compile", js.FuncOf(func(_ js.Value, args []js.Value) any {
		source := ""
		if len(args) > 0 {
			source = args[0].String()
		}
		out, _ := json.Marshal(compileBuffer(source))
		return string(out)
	}))
	js.Global().Set("hover", api)
	select {} // keep the Go runtime alive for later calls
}

// compileBuffer compiles one buffer against the bundled standard library.
func compileBuffer(source string) compileResult {
	fsys := fstest.MapFS{"work/main.hvr": &fstest.MapFile{Data: []byte(source)}}
	for path, f := range stdlib {
		fsys["stdlib/"+path] = f
	}
	return compile(fsys, entryPath, stdlibRoots(stdlib, stdlibPrefix))
}
