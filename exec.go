// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
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

const (
	// Similar to build tags, a magic comment of this form controls the
	// building and execution of the code snippet.
	magicComment = "//playground:"

	tagVersions  = "goversions" // Runs the binary across all of the listed versions
	tagBuildArgs = "buildargs"  // Builds the binary with the specified flags
	tagExecArgs  = "execargs"   // Executes the binary with the specified flags
	tagProfile   = "pprof"      // Runs pprof on the test; args are "cpu" and/or "mem"
)

// Communication with the executor is done by sending requests and receiving
// responses in the form of (action, data). The action is a short string
// indicating some action that the server or client should perform.
// The data is a string, the meaning of which is dependent on the action.
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
	reportProfile = "ReportProfile" // Server informs client about new profile; data is JSON dict with "name" and "id" fields
	statusStarted = "StatusStarted" // Server informs client that some action started; data is optional message
	statusUpdate  = "StatusUpdate"  // Server informs client about some on-going action; data is required message
	statusStopped = "StatusStopped" // Server informs client that some action stopped; data is optional message
)

type writerFunc func([]byte) (int, error)

func (wf writerFunc) Write(b []byte) (int, error) {
	return wf(b)
}

type executor struct {
	// blobStore is a synchronized map of MD5 hashes to binary blobs.
	bs   *blobStore
	bmu  sync.Mutex // Protects bids
	bids []string   // List of blob IDs to clear out

	// gc, fmt, and gcs are full paths to the go and gofmt binaries.
	gc  string            // Go binary to use
	fmt string            // Go formatter to use
	gcs map[string]string // Other Go versions available

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

func newExecutor(bs *blobStore, gcBin, fmtBin string, gcs map[string]string, sendMsg func(action, data string) error) *executor {
	tmpDir, err := ioutil.TempDir("", "sandbox")
	if err != nil {
		sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
	}

	ex := &executor{bs: bs, gc: gcBin, fmt: fmtBin, gcs: gcs, tmpDir: tmpDir, sendMsg: sendMsg}
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
	ex.deleteBlobs()
	os.RemoveAll(ex.tmpDir)
}

// deleteBlobs removes all blobs that this executor added to the blobStore.
func (ex *executor) deleteBlobs() {
	ex.bmu.Lock()
	for _, id := range ex.bids {
		ex.bs.Delete(id)
	}
	ex.bids = nil
	ex.bmu.Unlock()
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
	ex.deleteBlobs()

	// Parse the source file to determine some properties of it.
	if !ex.writeFile(tmpName, code) {
		return
	}
	hasMain, gcs, buildArgs, execArgs, profArgs, ok := ex.parseFile(filepath.Join(ex.tmpDir, tmpName))
	if !ok {
		return
	}
	verbose := len(gcs)+len(buildArgs)+len(execArgs)+len(profArgs) > 0

	// Setup the Go compiler version.
	if len(gcs) == 0 {
		gcs = []string{ex.gc}
	} else {
		if len(profArgs) > 0 {
			ex.sendMsg(statusUpdate, "WARNING: Support for profiling earlier Go versions is flaky!\n\n")
		}
		for i, gcName := range gcs {
			if gcBin, ok := ex.gcs[gcName]; ok {
				gcs[i] = gcBin
			} else {
				ex.sendMsg(statusUpdate, fmt.Sprintf("Unknown Go version: %v\n", gcName))
				return
			}
		}
	}

	// Setup arguments for performance profiling.
	if len(profArgs) > 0 {
		if len(execArgs) == 0 {
			execArgs = []string{"-test.v", "-test.run=-", "-test.bench=."}
		}
		for _, arg := range profArgs {
			switch arg {
			case "cpu":
				execArgs = append(execArgs, "-test.cpuprofile=cpu.prof")
			case "mem":
				execArgs = append(execArgs, "-test.memprofile=mem.prof")
			default:
				ex.sendMsg(statusUpdate, fmt.Sprintf("Unknown profiling argument: %v\n", arg))
				return
			}
		}
	}

	// Final adjustments on arguments for building and executing.
	var name string
	if hasMain {
		name = "main.go"
		buildArgs = append(append([]string{"build"}, buildArgs...), name)
		execArgs = append([]string{"./main"}, execArgs...)
	} else {
		name = "main_test.go"
		buildArgs = append(append([]string{"test", "-c"}, buildArgs...), name)
		if len(execArgs) == 0 {
			execArgs = []string{"./main.test", "-test.v", "-test.run=.", "-test.bench=."}
		} else {
			execArgs = append([]string{"./main.test"}, execArgs...)
		}
	}

	if err := os.Rename(filepath.Join(ex.tmpDir, tmpName), filepath.Join(ex.tmpDir, name)); err != nil {
		ex.sendMsg(statusUpdate, fmt.Sprintf("Unexpected error: %v\n", err))
		return
	}

	// Build and execute the source file for each go compiler versions.
	for _, gc := range gcs {
		// Check for cancelation.
		select {
		case <-ex.ctx.Done():
			return
		default:
		}

		if verbose {
			cmd := strings.Join(append([]string{gc}, buildArgs...), " ")
			ex.sendMsg(statusUpdate, fmt.Sprintf("Compiling program... (command: %v)\n", cmd))
		} else {
			ex.sendMsg(statusUpdate, "Compiling program...\n")
		}
		bb := new(bytes.Buffer)
		if !ex.runCommand(bb, append([]string{gc}, buildArgs...)...) {
			ex.reportBadLines(bb.Bytes())
			continue
		}

		// HACK: Go1.0 would output the test binary as different name from all
		// other versions of Go. Thus, we preemptively rename the old name to
		// the new one before running the test.
		os.Rename(filepath.Join(ex.tmpDir, "command-line-arguments.test"), filepath.Join(ex.tmpDir, "main.test"))

		if verbose {
			cmd := strings.Join(execArgs, " ")
			ex.sendMsg(statusUpdate, fmt.Sprintf("Starting program... (command: %v)\n", cmd))
		} else {
			ex.sendMsg(clearOutput, "")
		}
		if !ex.runCommand(ioutil.Discard, execArgs...) {
			ex.sendMsg(statusUpdate, "\n")
			continue
		}
		ex.sendMsg(statusUpdate, "Program exited.\n")

		if len(profArgs) > 0 {
			ex.processProfiles(profArgs)
		}
		ex.sendMsg(statusUpdate, "\n")
	}
}

// parseFile parses a Go source file and reports various properties:
//	hasMain: whether the file has a main function (as opposed to a test suite)
//	gcs: versions of Go to use; nil if not specified
//	buildArgs: custom build arguments; nil if not specified
//	execArgs: custom execution arguments; nil if not specified
//	profArgs: pprof modes to use (mem and/or cpu); nil if not specified
func (ex *executor) parseFile(file string) (hasMain bool, gcs, buildArgs, execArgs, profArgs []string, parseOk bool) {
	// Parse source file for package name and comments.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		parseOk = true // Best effort for parsing; allow build to report errors later
		return
	}
	if f.Name.Name != "main" {
		ex.sendMsg(statusUpdate, "Program must be in 'package main'.\n")
		return
	}
	var magics []string
	for _, cc := range f.Comments {
		for _, c := range cc.List {
			if strings.HasPrefix(c.Text, magicComment) {
				magics = append(magics, strings.TrimPrefix(c.Text, magicComment))
			}
		}
	}

	// Parse source file for function declarations.
	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		parseOk = true // Best effort for parsing; allow build to report errors later
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

	// Process magic comments.
	for _, c := range magics {
		args, ok := extractArgs(c)
		if !ok {
			ex.sendMsg(statusUpdate, fmt.Sprintf("Unable to parse magic comment: %q", magicComment+c))
			return
		}
		switch args[0] {
		case tagVersions:
			gcs = args[1:]
		case tagBuildArgs:
			buildArgs = args[1:]
		case tagExecArgs:
			execArgs = args[1:]
		case tagProfile:
			profArgs = args[1:]
		default:
			ex.sendMsg(statusUpdate, fmt.Sprintf("Unknown magic comment: %q", magicComment+c))
			return
		}
	}
	if !hasTests && len(profArgs) > 0 {
		ex.sendMsg(statusUpdate, "Profiling is only available on test suites")
		return
	}
	return hasMain, gcs, buildArgs, execArgs, profArgs, true
}

// processProfiles generates SVG and HTML files for the pprof profiles
// generated by go test. It stores the output files in blobStore and informs
// the client of the profiles by sending reportProfile messages to the client.
func (ex *executor) processProfiles(profArgs []string) {
	ex.sendMsg(statusUpdate, "Generating performance reports...\n")
	defer ex.sendMsg(statusUpdate, "Report generation done.\n")

	// HACK: The "-output" flag on pprof does not properly work in some cases,
	// which makes it very hard to properly retrieve processed profiles as
	// raw files. One thing that seems to work consistently well is the display
	// of a given profile into the browser. As such, we use a trivial program
	// "prof_copy" that acts as a "browser" and simply writes input to a file.
	//
	// See http://golang.org/issue/16179.

	// Build the shady prof_copy program.
	const profCopy = `
		package main
		import "io"
		import "os"
		func main() {
			src, _ := os.Open(os.Args[2])
			defer src.Close()
			dst, _ := os.Create(os.Args[1])
			defer dst.Close()
			io.Copy(dst, src)
		}
	`
	if !ex.writeFile("prof_copy.go", profCopy) {
		return
	}
	if !ex.runCommand(ioutil.Discard, ex.gc, "build", "prof_copy.go") {
		return // Should not fail
	}

	runProf := func(output string, args ...string) {
		// HACK: We use the default Go toolchain to generate the pprof reports
		// for all Go binaries regardless of which Go version compiled it.
		// This is incorrect, but easier than trying to have a set of flags that
		// works for every Go version thus far. The arguments used here assume
		// that it is for a relatively newer version of Go (1.6 and higher).
		cmd := exec.CommandContext(ex.ctx, ex.gc, append([]string{"tool", "pprof"}, args...)...)
		cmd.Dir = ex.tmpDir
		cmd.Env = append(cmd.Env, fmt.Sprintf("PPROF_TMPDIR=%s", ex.tmpDir))
		cmd.Env = append(cmd.Env, fmt.Sprintf("BROWSER=%s %s", filepath.Join(ex.tmpDir, "prof_copy"), output))
		cmd.Env = append(cmd.Env, os.Environ()...)
		if err := cmd.Run(); err != nil {
			ex.sendMsg(statusUpdate, fmt.Sprintf("\tDropped report: %s (unexpected error: %v)\n", output, err))
			return
		}

		b, _ := ioutil.ReadFile(filepath.Join(ex.tmpDir, output))
		if len(b) > 1<<24 {
			ex.sendMsg(statusUpdate, fmt.Sprintf("\tDropped report: %s (file too large: %d bytes)\n", output, len(b)))
		} else if len(b) > 0 {
			var mime string
			if strings.HasSuffix(output, ".svg") {
				mime = "image/svg+xml"
			}
			if strings.HasSuffix(output, ".html") {
				mime = "text/html"
			}

			id := ex.bs.Insert(blob{data: b, mime: mime})
			ex.mu.Lock()
			ex.bids = append(ex.bids, id) // Make sure executor knows to delete this later
			ex.mu.Unlock()

			b, _ = json.Marshal(map[string]string{"name": output, "id": id})
			ex.sendMsg(reportProfile, string(b))
		}
	}

	// Create all relevant profiles.
	for _, arg := range profArgs {
		switch arg {
		case "cpu":
			runProf("cpu_graph.svg", "-web", "main.test", "cpu.prof")
			runProf("cpu_list.html", "-weblist=.", "main.test", "cpu.prof")
		case "mem":
			runProf("mem_objects_graph.svg", "-alloc_objects", "-web", "main.test", "mem.prof")
			runProf("mem_objects_list.html", "-alloc_objects", "-weblist=.", "main.test", "mem.prof")
			runProf("mem_space_graph.svg", "-alloc_space", "-web", "main.test", "mem.prof")
			runProf("mem_space_list.html", "-alloc_space", "-weblist=.", "main.test", "mem.prof")
		}
	}
}

// extractArgs splits str across whitespaces, but is able to understand
// tokens that are quoted strings (according to Go syntax).
func extractArgs(str string) ([]string, bool) {
	var ss []string
	input := strings.TrimSpace(str)
	for len(input) > 0 {
		var s string
		r := strings.NewReader(input)
		if _, err := fmt.Fscanf(r, "%s", &s); err != nil {
			return nil, false
		}
		if len(s) > 0 && s[0] == '"' {
			r = strings.NewReader(input)
			if _, err := fmt.Fscanf(r, "%q", &s); err != nil {
				return nil, false
			}
		}
		input = input[len(input)-r.Len():]
		ss = append(ss, s)
	}
	if len(ss) == 0 || !strings.HasPrefix(str, ss[0]) {
		return nil, false
	}
	return ss, true
}

type blob struct {
	data []byte
	mime string
}

type blobStore struct {
	mu sync.Mutex
	m  map[string]blob
}

func newBlobStore() *blobStore {
	return &blobStore{m: make(map[string]blob)}
}

func (bs *blobStore) Insert(b blob) (id string) {
	h := md5.Sum(b.data) // Assume MIME doesn't change for given data
	id = hex.EncodeToString(h[:])
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.m[id] = b
	return id
}

func (bs *blobStore) Retrieve(id string) blob {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.m[id]
}

func (bs *blobStore) Delete(id string) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	delete(bs.m, id)
}

func (bs *blobStore) Len() int {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return len(bs.m)
}
