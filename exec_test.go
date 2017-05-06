// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const reMagic = "RE> "

type message struct {
	action, data string // If data starts with a "RE> ", then it is a regexp
}

func TestExecutor(t *testing.T) {
	var mu sync.Mutex // Protects sendMsg
	var sendMsg func(action, data string)
	sendMsgProxy := func(action, data string) error {
		mu.Lock()
		sendMsg(action, data)
		mu.Unlock()
		return nil
	}

	bs := newBlobStore()
	gcs := map[string]string{"go-alpha": "go", "go-beta": "go"}
	ex := newExecutor(bs, "go", "gofmt", gcs, sendMsgProxy)
	defer ex.Close()

	var tsub *testing.T            // Updated for each sub-test
	next := make(chan struct{}, 1) // Event triggered at end of each test

	// wantMessages creates a function callback for checking new messages that
	// they match the wanted messages. The callback will automatically send
	// on next when done.
	wantMessages := func(want []message) func(action, data string) {
		var got []message
		return func(action, data string) {
			isAppend := action == appendStdout || action == appendStderr
			if len(got) > 0 && got[len(got)-1].action == action && isAppend {
				got[len(got)-1].data += data
			} else {
				got = append(got, message{action, data})
			}

			if !isAppend && len(got) == len(want) {
				if !equalMessages(got, want) {
					tsub.Errorf("mismatching messages:\ngot  %q\nwant %q", got, want)
				}
				next <- struct{}{} // Inform that the test is done
			}
			if len(got) > len(want) {
				tsub.Errorf("got unexpected message{action: %s, data: %q}", action, data)
			}

		}
	}

	tests := []struct {
		label string // Name of the test

		action string
		data   string

		// recvMsg callback to use to check the executor state.
		// The function should call t.Error instead of t.Fatal.
		// After the test is done, this must send on the next chan exactly once.
		recvMsg func(action, data string)
	}{{
		label:  "StopNoop",
		action: actionStop,
	}, {
		label:  "FormatValid",
		action: actionFormat,
		data:   `package main;import "fmt"; func main() { fmt.Println("Hello, world!") }`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Formatting source...\n"},
			{actionFormat, "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"Hello, world!\") }\n"},
			{clearOutput, ""},
			{statusUpdate, "Source formatted.\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "FormatInvalid",
		action: actionFormat,
		data:   "package main\n\n\nnot valid go",
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Formatting source...\n"},
			{appendStderr, "RE> main.go:4:1:.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[4]"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunInvalid",
		action: actionRun,
		data:   "package main\n\n\nnot valid go",
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{appendStderr, "RE> main_test.go:4:1:.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[4]"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunValid1",
		action: actionRun,
		data:   `package main; import "fmt"; import "os"; func main() { fmt.Fprintln(os.Stderr, "stderr") }`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStderr, "stderr\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunValid2",
		action: actionRun,
		data:   `package main; import "fmt"; func main() { for i := range make([]int, 10) { fmt.Println(i) } }`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStdout, "0\n1\n2\n3\n4\n5\n6\n7\n8\n9\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunBadPackage",
		action: actionRun,
		data:   `package foo; func main(){}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Program must be in 'package main'.\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunBadMain",
		action: actionRun,
		data:   `package main; func Main(){}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Program must have either a main function or a set of test functions.\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunTest",
		action: actionRun,
		data:   `package main; import "testing"; func Test(t *testing.T){t.Error("test error")}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStdout, "RE> FAIL: Test(.*|\n)*test error\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "RunForever",
		action: actionRun,
		data:   `package main; import "time"; func main() { time.Sleep(time.Hour) }`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
		}),
	}, {
		label:  "StopPrevious",
		action: actionStop,
		recvMsg: wantMessages([]message{
			{statusUpdate, "RE> Unexpected error:.*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaBadVersions",
		action: actionRun,
		data: `//playground:goversions go-bad
			package main; func main() {}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unknown Go version: go-bad\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaBadPProfArgs",
		action: actionRun,
		data: `//playground:pprof mode-bad
			package main; import "testing"; func Benchmark(b *testing.B) {}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unknown profiling argument: mode-bad\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaBadSyntax",
		action: actionRun,
		data: `//playground:arg0 "arg2...
			package main; func main(){}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unable to parse magic comment: \"//playground:arg0 \\\"arg2...\""},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaUnknown",
		action: actionRun,
		data: `//playground:unknown arg1 arg2 arg3
			package main; func main(){}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unknown magic comment: \"//playground:unknown arg1 arg2 arg3\""},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaBadPProfUsage",
		action: actionRun,
		data: `//playground:pprof cpu mem
			package main; func main(){}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Profiling is only available on test suites"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaVersions",
		action: actionRun,
		data: `//playground:goversions go-alpha go-beta
			package main; import "fmt"; func main() { fmt.Println("hello") }`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program... (command: go build main.go)\n"},
			{statusUpdate, "Starting program... (command: ./main)\n"},
			{appendStdout, "hello\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusUpdate, "Compiling program... (command: go build main.go)\n"},
			{statusUpdate, "Starting program... (command: ./main)\n"},
			{appendStdout, "hello\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaBuildArgs",
		action: actionRun,
		data: `//playground:buildargs -race
			package main
			func main() {
				var x int // Race over this variable
				c := make(chan bool)
				for i := 0; i < 10; i++ {
					go func(i int) { x = i; c<-true }(i)
				}
				for i := 0; i < 10; i++ { <-c }
			}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program... (command: go build -race main.go)\n"},
			{statusUpdate, "Starting program... (command: ./main)\n"},
			{appendStderr, "RE> WARNING: DATA RACE"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaExecArgs",
		action: actionRun,
		data: `//playground:execargs -myflag=1337
			package main
			import "fmt"
			import "flag"
			func main() {
				x := flag.Int("myflag", 0, "")
				flag.Parse()
				fmt.Println(*x)
			}`,
		recvMsg: wantMessages([]message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program... (command: go build main.go)\n"},
			{statusUpdate, "Starting program... (command: ./main -myflag=1337)\n"},
			{appendStdout, "1337\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}, {
		label:  "PragmaPProfArgs",
		action: actionRun,
		data: `//playground:pprof cpu mem
				package main
				import "testing"
				import "strings"
				var sink []string
				func Benchmark(b *testing.B) {
					d := strings.Repeat("x ☺ ", 1<<20)
					for i:= 0; i < b.N; i++ {
						sink = strings.Fields(d)
					}
				}`,
		// This takes a while because it runs benchmarks and generates profiles,
		// which sometimes are not generated if the test was too short.
		// Thus, we only test that we were able to see at least one profile.
		recvMsg: func() func(action, data string) {
			var hasStarted, hasProfile, hasStopped bool
			return func(action, data string) {
				switch {
				case !hasStarted:
					if action == statusStarted {
						hasStarted = true
					}
				case !hasProfile:
					if action == reportProfile {
						if !strings.Contains(data, "name") || !strings.Contains(data, "id") {
							tsub.Errorf("invalid reportProfile: %v", data)
						}
						hasProfile = true
					}
				case !hasStopped:
					if action == statusStopped {
						next <- struct{}{}
						hasStopped = true
					}
				default:
					tsub.Errorf("got unexpected message{action: %s, data: %q}", action, data)
				}
			}
		}(),
	}}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			// Setup the current sendMsg and tsub for the given test.
			mu.Lock()
			tsub = t
			sendMsg = tt.recvMsg
			if tt.recvMsg == nil {
				sendMsg = func(_, _ string) {} // Avoid nil panic
			}
			mu.Unlock()

			switch tt.action {
			case actionFormat, actionRun:
				ex.Start(tt.action, tt.data)
			case actionStop:
				ex.Stop()
			default:
				t.Fatalf("unknown action: %s", tt.action)
			}

			// Wait until this test is done.
			if tt.recvMsg != nil {
				select {
				case <-next:
					if t.Failed() {
						t.Fatalf("failed test")
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("timed out")
				}
			}
		})
	}

	// Test resource cleanup.
	ex.Close()
	if _, err := os.Stat(ex.tmpDir); err == nil {
		t.Errorf("unexpected Stat(%q) success", ex.tmpDir)
	}
	if n := bs.Len(); n > 0 {
		t.Errorf("unexpected non-empty blobStore: got %d blobs", n)
	}
}

func equalMessages(x, y []message) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		vx, vy := x[i], y[i]
		if vx.action != vy.action {
			return false
		}
		rx := strings.HasPrefix(vx.data, reMagic)
		ry := strings.HasPrefix(vy.data, reMagic)
		switch {
		case rx && ry:
			return false // Both can't be regexps
		case rx && !ry:
			r := strings.TrimPrefix(vx.data, reMagic)
			if !regexp.MustCompile(r).MatchString(vy.data) {
				return false
			}
		case !rx && ry:
			r := strings.TrimPrefix(vy.data, reMagic)
			if !regexp.MustCompile(r).MatchString(vx.data) {
				return false
			}
		case !rx && !ry:
			if vx.data != vy.data {
				return false
			}
		}
	}
	return true
}
