// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package errorstest

import (
	"bytes"
	"cmd/internal/quoted"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// A manually modified object file could pass unexpected characters
// into the files generated by cgo.

const magicInput = "abcdefghijklmnopqrstuvwxyz0123"
const magicReplace = "\n//go:cgo_ldflag \"-badflag\"\n//"

const cSymbol = "BadSymbol" + magicInput + "Name"
const cDefSource = "int " + cSymbol + " = 1;"
const cRefSource = "extern int " + cSymbol + "; int F() { return " + cSymbol + "; }"

// goSource is the source code for the trivial Go file we use.
// We will replace TMPDIR with the temporary directory name.
const goSource = `
package main

// #cgo LDFLAGS: TMPDIR/cbad.o TMPDIR/cbad.so
// extern int F();
import "C"

func main() {
	println(C.F())
}
`

func TestBadSymbol(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)

	dir := t.TempDir()

	mkdir := func(base string) string {
		ret := filepath.Join(dir, base)
		if err := os.Mkdir(ret, 0755); err != nil {
			t.Fatal(err)
		}
		return ret
	}

	cdir := mkdir("c")
	godir := mkdir("go")

	makeFile := func(mdir, base, source string) string {
		ret := filepath.Join(mdir, base)
		if err := os.WriteFile(ret, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
		return ret
	}

	cDefFile := makeFile(cdir, "cdef.c", cDefSource)
	cRefFile := makeFile(cdir, "cref.c", cRefSource)

	ccCmd := cCompilerCmd(t)

	cCompile := func(arg, base, src string) string {
		out := filepath.Join(cdir, base)
		run := append(ccCmd, arg, "-o", out, src)
		output, err := exec.Command(run[0], run[1:]...).CombinedOutput()
		if err != nil {
			t.Log(run)
			t.Logf("%s", output)
			t.Fatal(err)
		}
		if err := os.Remove(src); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Build a shared library that defines a symbol whose name
	// contains magicInput.

	cShared := cCompile("-shared", "c.so", cDefFile)

	// Build an object file that refers to the symbol whose name
	// contains magicInput.

	cObj := cCompile("-c", "c.o", cRefFile)

	// Rewrite the shared library and the object file, replacing
	// magicInput with magicReplace. This will have the effect of
	// introducing a symbol whose name looks like a cgo command.
	// The cgo tool will use that name when it generates the
	// _cgo_import.go file, thus smuggling a magic //go:cgo_ldflag
	// pragma into a Go file. We used to not check the pragmas in
	// _cgo_import.go.

	rewrite := func(from, to string) {
		obj, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}

		if bytes.Count(obj, []byte(magicInput)) == 0 {
			t.Fatalf("%s: did not find magic string", from)
		}

		if len(magicInput) != len(magicReplace) {
			t.Fatalf("internal test error: different magic lengths: %d != %d", len(magicInput), len(magicReplace))
		}

		obj = bytes.ReplaceAll(obj, []byte(magicInput), []byte(magicReplace))

		if err := os.WriteFile(to, obj, 0644); err != nil {
			t.Fatal(err)
		}
	}

	cBadShared := filepath.Join(godir, "cbad.so")
	rewrite(cShared, cBadShared)

	cBadObj := filepath.Join(godir, "cbad.o")
	rewrite(cObj, cBadObj)

	goSourceBadObject := strings.ReplaceAll(goSource, "TMPDIR", godir)
	makeFile(godir, "go.go", goSourceBadObject)

	makeFile(godir, "go.mod", "module badsym")

	// Try to build our little package.
	cmd := exec.Command(testenv.GoToolPath(t), "build", "-ldflags=-v")
	cmd.Dir = godir
	output, err := cmd.CombinedOutput()

	// The build should fail, but we want it to fail because we
	// detected the error, not because we passed a bad flag to the
	// C linker.

	if err == nil {
		t.Errorf("go build succeeded unexpectedly")
	}

	t.Logf("%s", output)

	for _, line := range bytes.Split(output, []byte("\n")) {
		if bytes.Contains(line, []byte("dynamic symbol")) && bytes.Contains(line, []byte("contains unsupported character")) {
			// This is the error from cgo.
			continue
		}

		// We passed -ldflags=-v to see the external linker invocation,
		// which should not include -badflag.
		if bytes.Contains(line, []byte("-badflag")) {
			t.Error("output should not mention -badflag")
		}

		// Also check for compiler errors, just in case.
		// GCC says "unrecognized command line option".
		// clang says "unknown argument".
		if bytes.Contains(line, []byte("unrecognized")) || bytes.Contains(output, []byte("unknown")) {
			t.Error("problem should have been caught before invoking C linker")
		}
	}
}

func cCompilerCmd(t *testing.T) []string {
	cc, err := quoted.Split(goEnv(t, "CC"))
	if err != nil {
		t.Skipf("parsing go env CC: %s", err)
	}
	if len(cc) == 0 {
		t.Skipf("no C compiler")
	}
	testenv.MustHaveExecPath(t, cc[0])

	out := goEnv(t, "GOGCCFLAGS")
	quote := '\000'
	start := 0
	lastSpace := true
	backslash := false
	s := string(out)
	for i, c := range s {
		if quote == '\000' && unicode.IsSpace(c) {
			if !lastSpace {
				cc = append(cc, s[start:i])
				lastSpace = true
			}
		} else {
			if lastSpace {
				start = i
				lastSpace = false
			}
			if quote == '\000' && !backslash && (c == '"' || c == '\'') {
				quote = c
				backslash = false
			} else if !backslash && quote == c {
				quote = '\000'
			} else if (quote == '\000' || quote == '"') && !backslash && c == '\\' {
				backslash = true
			} else {
				backslash = false
			}
		}
	}
	if !lastSpace {
		cc = append(cc, s[start:])
	}

	// Force reallocation (and avoid aliasing bugs) for tests that append to cc.
	cc = cc[:len(cc):len(cc)]

	return cc
}

func goEnv(t *testing.T, key string) string {
	out, err := exec.Command("go", "env", key).CombinedOutput()
	if err != nil {
		t.Logf("go env %s\n", key)
		t.Logf("%s", out)
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
