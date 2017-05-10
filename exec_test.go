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

// messageTester assists in checking the messages sent from the executor.
type messageTester struct {
	// After the test is done, an event is sent on the channel.
	// Each test should only send at most one event.
	Next chan struct{}

	mu        sync.Mutex // Protects t, got, want
	t         *testing.T
	got, want []message
	checkFunc func(action, data string)
}

func newMessageTester(t *testing.T) *messageTester {
	return &messageTester{Next: make(chan struct{}, 1), t: t}
}

// SetT atomically sets the T object for this tester.
func (mt *messageTester) SetT(t *testing.T) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.t = t
}

// Errorf calls t.Errorf, but is protected by a mutex.
func (mt *messageTester) Errorf(format string, args ...interface{}) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.t.Errorf(format, args...)
}

// WantMessages registers a set of messages as the expected messages to be sent.
// Upon completion, this will send an event on the Next channel.
func (mt *messageTester) WantMessages(want []message) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.want, mt.got, mt.checkFunc = want, nil, nil
}

// MessageChecker registers a function callback that is used to validate
// all future sent messages. It is the responsibility of the callback to
// send an event on the Next channel when finished.
func (mt *messageTester) MessageChecker(f func(action, data string)) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.want, mt.got, mt.checkFunc = nil, nil, f
}

// SendMessage mocks sending a message and checks the message for correctness.
// Pass this function to newExecutor to check its outputs.
func (mt *messageTester) SendMessage(action, data string) error {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	if mt.checkFunc != nil {
		mt.checkFunc(action, data)
		return nil
	}

	isAppend := action == appendStdout || action == appendStderr
	if len(mt.got) > 0 && mt.got[len(mt.got)-1].action == action && isAppend {
		mt.got[len(mt.got)-1].data += data
	} else {
		mt.got = append(mt.got, message{action, data})
	}

	if !isAppend && len(mt.got) == len(mt.want) {
		if !equalMessages(mt.got, mt.want) {
			mt.Errorf("mismatching messages:\ngot  %q\nwant %q", mt.got, mt.want)
		}
		mt.Next <- struct{}{} // Inform that the test is done
	}
	if len(mt.got) > len(mt.want) {
		mt.Errorf("got unexpected message{action: %s, data: %q}", action, data)
	}
	return nil
}

func TestExecutor(t *testing.T) {
	mt := newMessageTester(t)
	bs := newBlobStore()
	gcs := map[string]string{"go-alpha": "go", "go-beta": "go"}
	ex := newExecutor(bs, "go", "gofmt", gcs, mt.SendMessage)
	defer ex.Close()

	tests := []struct {
		label string // Name of the test
		long  bool   // Does this test take a long time?

		action string
		data   string

		// Either want or check will be set.
		want  []message                 // List of expected messages
		check func(action, data string) // Callback function to check each message
	}{{
		label:  "StopNoop",
		action: actionStop,
	}, {
		label:  "FormatValid",
		action: actionFormat,
		data:   `package main;import "fmt"; func main() { fmt.Println("Hello, world!") }`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Formatting source...\n"},
			{actionFormat, "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"Hello, world!\") }\n"},
			{clearOutput, ""},
			{statusUpdate, "Source formatted.\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "FormatInvalid",
		action: actionFormat,
		data:   "package main\n\n\nnot valid go",
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Formatting source...\n"},
			{appendStderr, "RE> main.go:4:1:.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[4]"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunInvalid",
		action: actionRun,
		data:   "package main\n\n\nnot valid go",
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{appendStderr, "RE> main_test.go:4:1:.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[4]"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunValid1",
		action: actionRun,
		data:   `package main; import "fmt"; import "os"; func main() { fmt.Fprintln(os.Stderr, "stderr") }`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStderr, "stderr\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunValid2",
		action: actionRun,
		data:   `package main; import "fmt"; func main() { for i := range make([]int, 10) { fmt.Println(i) } }`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStdout, "0\n1\n2\n3\n4\n5\n6\n7\n8\n9\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunBadPackage",
		action: actionRun,
		data:   `package foo; func main(){}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Program must be in 'package main'.\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunBadMain",
		action: actionRun,
		data:   `package main; func Main(){}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Program must have either a main function or a set of test functions.\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunTest",
		action: actionRun,
		data:   `package main; import "testing"; func Test(t *testing.T){t.Error("test error")}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStdout, "RE> FAIL: Test(.*|\n)*test error\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunForever",
		action: actionRun,
		data:   `package main; import "time"; func main() { time.Sleep(time.Hour) }`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
		},
	}, {
		label:  "StopPrevious",
		action: actionStop,
		want: []message{
			{statusUpdate, "RE> Unexpected error:.*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaBadVersions",
		action: actionRun,
		data: `//playground:goversions go-bad
			package main; func main() {}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unknown Go version: go-bad\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaBadPProfArgs",
		action: actionRun,
		data: `//playground:pprof mode-bad
			package main; import "testing"; func Benchmark(b *testing.B) {}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unknown profiling argument: mode-bad\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaBadSyntax",
		action: actionRun,
		data: `//playground:arg0 "arg2...
			package main; func main(){}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unable to parse magic comment: \"//playground:arg0 \\\"arg2...\""},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaUnknown",
		action: actionRun,
		data: `//playground:unknown arg1 arg2 arg3
			package main; func main(){}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Unknown magic comment: \"//playground:unknown arg1 arg2 arg3\""},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaBadPProfUsage",
		action: actionRun,
		data: `//playground:pprof cpu mem
			package main; func main(){}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Profiling is only available on test suites"},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaVersions",
		action: actionRun,
		data: `//playground:goversions go-alpha go-beta
			package main; import "fmt"; func main() { fmt.Println("hello") }`,
		want: []message{
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
		},
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
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program... (command: go build -race main.go)\n"},
			{statusUpdate, "Starting program... (command: ./main)\n"},
			{appendStderr, "RE> WARNING: DATA RACE"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		},
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
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program... (command: go build main.go)\n"},
			{statusUpdate, "Starting program... (command: ./main -myflag=1337)\n"},
			{appendStdout, "1337\n"},
			{statusUpdate, "Program exited.\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		},
	}, {
		label:  "PragmaPProfArgs",
		long:   true,
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
		check: func() func(action, data string) {
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
							mt.Errorf("invalid reportProfile: %v", data)
						}
						hasProfile = true
					}
				case !hasStopped:
					if action == statusStopped {
						mt.Next <- struct{}{}
						hasStopped = true
					}
				default:
					mt.Errorf("got unexpected message{action: %s, data: %q}", action, data)
				}
			}
		}(),
	}}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			if testing.Short() && tt.long {
				t.SkipNow()
			}

			mt.SetT(t)
			switch {
			case tt.want != nil:
				mt.WantMessages(tt.want)
			case tt.check != nil:
				mt.MessageChecker(tt.check)
			default:
				mt.Next <- struct{}{} // Don't block waiting for results
			}

			switch tt.action {
			case actionFormat, actionRun:
				ex.Start(tt.action, tt.data)
			case actionStop:
				ex.Stop()
			default:
				t.Fatalf("unknown action: %s", tt.action)
			}

			// Wait until this test is done.
			select {
			case <-mt.Next:
				if t.Failed() {
					t.Fatalf("failed test")
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("timed out")
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
