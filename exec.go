// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Communication with the executor is done by sending requests and receiving
// responses in the form of (action, data). Where action is a short string
// indicating some action that the server or client should perform.
// The data is a string, the meaning of which is depending on the action.
//
// These constants define all possible actions.
const (
	// Sent by client to server.
	actionFormat = "Format" // Server formats the Go source in the data
	actionRun    = "Run"    // Server runs the Go source in the data
	actionStop   = "Stop"   // Stop any on-going format or run actions

	// Sent by server to client.
	clearOutput   = "ClearOutput"   // Client clears the output console; has no data
	markLines     = "MarkLines"     // Client highlights the specified lines; data is JSON list of integers
	appendStdout  = "AppendStdout"  // Client appends the data as stdout from the server's action
	appendStderr  = "AppendStderr"  // Client appends the data as stderr from the server's action
	statusStarted = "StatusStarted" // Server informs client that some action started; data is optional message
	statusUpdate  = "StatusUpdate"  // Server informs client about some on-going action; data is required message
	statusStopped = "StatusStopped" // Server informs client that some action stopped; data is optional message
)

type writerFunc func([]byte) (int, error)

func (wf writerFunc) Write(b []byte) (int, error) {
	return wf(b)
}

type executor struct {
	// gc and fmt are full paths to the go and gofmt binaries.
	gc  string // Go binary to use
	fmt string // Go formatter to use

	// tmpDir is a temporary directory to use for running binaries.
	tmpDir string

	// sendMsg is a callback for the server to send (action, data) messages
	// back to the client.
	sendMsg func(action, data string) error

	// stdout and stderr are thin wrappers around sendMsg for sending
	// appendStdout and appendStderr messages to the client.
	stdout io.Writer
	stderr io.Writer

	mu     sync.Mutex // Protects closed, ctx, and cancel
	closed bool
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newExecutor(gcBin, fmtBin string, sendMsg func(action, data string) error) *executor {
	tmpDir, err := ioutil.TempDir("", "sandbox")
	if err != nil {
		sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
	}

	ex := &executor{gc: gcBin, fmt: fmtBin, tmpDir: tmpDir, sendMsg: sendMsg}
	ex.stdout = writerFunc(func(b []byte) (int, error) {
		return len(b), sendMsg(appendStdout, string(b))
	})
	ex.stderr = writerFunc(func(b []byte) (int, error) {
		return len(b), sendMsg(appendStderr, string(b))
	})
	ex.ctx, ex.cancel = context.WithCancel(context.Background())
	return ex
}

// Start handles either the format or run actions on some given Go source code.
// If there is already an on-going action, then this stops that action before
// preceding with the new action.
func (ex *executor) Start(action, data string) {
	ex.Stop() // In case the previous task is still running

	// Setup a new context for canceling the upcoming task.
	ex.mu.Lock()
	if ex.closed {
		ex.sendMsg(statusUpdate, "Unexpected error: server is shutdown\n")
		return
	}
	ex.ctx, ex.cancel = context.WithCancel(context.Background())
	ex.wg.Add(1) // Done is called either in handleFormat or handleRun
	ex.mu.Unlock()

	switch action {
	case actionFormat:
		ex.sendMsg(statusStarted, "")
		go ex.handleFormat(data)
	case actionRun:
		ex.sendMsg(statusStarted, "")
		go ex.handleRun(data)
	default:
		ex.sendMsg(statusUpdate, fmt.Sprintf("Unknown action: %s\n", action))
		ex.wg.Done()
	}
}

// Stop cancels any on-going tasks and blocks until all tasks have stopped.
func (ex *executor) Stop() {
	ex.mu.Lock()
	ex.cancel()
	ex.mu.Unlock()
	ex.wg.Wait()
}

// Close stops any on-going tasks and releases any used resources.
func (ex *executor) Close() {
	ex.mu.Lock()
	ex.closed = true
	ex.cancel()
	ex.mu.Unlock()
	ex.wg.Wait()
	os.RemoveAll(ex.tmpDir)
}

// runCommand runs an arbitrary command in args and returns true if successful.
// The stderr of the process is also captured and written to w.
func (ex *executor) runCommand(w io.Writer, args ...string) bool {
	cmd := exec.CommandContext(ex.ctx, args[0], args[1:]...)
	cmd.Dir = ex.tmpDir
	cmd.Stdout = ex.stdout
	cmd.Stderr = io.MultiWriter(ex.stderr, w)
	if err := cmd.Run(); err != nil {
		ex.sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
		return false
	}
	return true
}

// Regexp for parsing out line numbers from the stderr of go build.
// This works on all versions of Go (current latest release is 1.8).
var reLine = regexp.MustCompile(`^(\./)?main(_test)?\.go:(\d+)`)

// reportBadLines parses the stderr of a go build for all offending lines.
func (ex *executor) reportBadLines(b []byte) {
	var lines []int
	for _, s := range strings.Split(string(b), "\n") {
		if m := reLine.FindString(s); m != "" {
			m = m[strings.Index(m, ":")+1:]
			i, _ := strconv.Atoi(m)
			lines = append(lines, i)
		}
	}
	if len(lines) > 0 {
		b, _ := json.Marshal(lines)
		ex.sendMsg(markLines, string(b))
	}
}

func (ex *executor) readFile(name string) (string, bool) {
	b, err := ioutil.ReadFile(filepath.Join(ex.tmpDir, name))
	if err != nil {
		ex.sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
		return "", false
	}
	return string(b), true
}

func (ex *executor) writeFile(name, data string) bool {
	if err := ioutil.WriteFile(filepath.Join(ex.tmpDir, name), []byte(data), 0664); err != nil {
		ex.sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
		return false
	}
	return true
}

func (ex *executor) handleFormat(code string) {
	defer ex.wg.Done()
	defer ex.sendMsg(statusStopped, "")

	// Format the input source.
	ex.sendMsg(clearOutput, "")
	ex.sendMsg(statusUpdate, "Formatting source...\n")
	if !ex.writeFile("main.go", code) {
		return
	}
	bb := new(bytes.Buffer)
	if !ex.runCommand(bb, ex.fmt, "-w", "main.go") {
		ex.reportBadLines(bb.Bytes())
		return
	}

	// Send the formatted source back to client.
	code, ok := ex.readFile("main.go")
	if !ok {
		return
	}
	ex.sendMsg(actionFormat, code)
	ex.sendMsg(clearOutput, "")
	ex.sendMsg(statusUpdate, "Source formatted.\n")
}

func (ex *executor) handleRun(code string) {
	const tmpName = "temp.go"

	defer ex.wg.Done()
	defer ex.sendMsg(statusStopped, "")
	ex.sendMsg(clearOutput, "")

	// Best effort at clearing out directory and stale data.
	fis, _ := ioutil.ReadDir(ex.tmpDir)
	for _, fi := range fis {
		os.RemoveAll(filepath.Join(ex.tmpDir, fi.Name()))
	}

	// Parse the source file to determine some properties of it.
	if !ex.writeFile(tmpName, code) {
		return
	}
	hasMain, ok := ex.parseFile(filepath.Join(ex.tmpDir, tmpName))
	if !ok {
		return
	}

	// Arguments for building and executing.
	var name string
	var buildArgs, execArgs []string
	if hasMain {
		name = "main.go"
		buildArgs = []string{"build", name}
		execArgs = []string{"./main"}
	} else {
		name = "main_test.go"
		buildArgs = []string{"test", "-c", name}
		execArgs = []string{"./main.test"}
	}

	if err := os.Rename(filepath.Join(ex.tmpDir, tmpName), filepath.Join(ex.tmpDir, name)); err != nil {
		ex.sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
		return
	}

	// Build and execute the source file.
	ex.sendMsg(statusUpdate, "Compiling program...\n")
	bb := new(bytes.Buffer)
	if !ex.runCommand(bb, append([]string{ex.gc}, buildArgs...)...) {
		ex.reportBadLines(bb.Bytes())
		return
	}

	// HACK: Go1.0 would output the test binary as different name from all
	// other versions of Go. Thus, we preemptively rename the old name to
	// the new one before running the test.
	os.Rename(filepath.Join(ex.tmpDir, "command-line-arguments.test"), filepath.Join(ex.tmpDir, "main.test"))

	ex.sendMsg(clearOutput, "")
	if !ex.runCommand(ioutil.Discard, execArgs...) {
		ex.sendMsg(statusUpdate, "\n")
		return
	}
	ex.sendMsg(statusUpdate, "Program exited.\n")
}

func (ex *executor) parseFile(file string) (hasMain bool, ok bool) {
	// Parse source file for package name and comments.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		ok = true // Best effort for parsing; allow build to report errors later
		return
	}
	if f.Name.Name != "main" {
		ex.sendMsg(statusUpdate, "Program must be in 'package main'.\n")
		return
	}

	// Parse source file for function declarations.
	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		ok = true // Best effort for parsing; allow build to report errors later
		return
	}
	var hasTests bool
	for _, dd := range f.Decls {
		if fd, ok := dd.(*ast.FuncDecl); ok {
			hasMain = hasMain || (fd.Recv == nil && fd.Name.Name == "main" &&
				(fd.Type.Params == nil || fd.Type.Params.NumFields() == 0) &&
				(fd.Type.Results == nil || fd.Type.Results.NumFields() == 0))
			hasTests = hasTests || (fd.Recv == nil &&
				(strings.HasPrefix(fd.Name.Name, "Benchmark") || strings.HasPrefix(fd.Name.Name, "Test")) &&
				(fd.Type.Params != nil && fd.Type.Params.NumFields() == 1) &&
				(fd.Type.Results == nil || fd.Type.Results.NumFields() == 0))
		}
	}
	if hasMain == hasTests {
		ex.sendMsg(statusUpdate, "Program must have either a main function or a set of test functions.\n")
		return
	}
	return hasMain, true
}
