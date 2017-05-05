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

	ex := newExecutor("go", "gofmt", sendMsgProxy)
	defer ex.Close()

	next := make(chan struct{}, 1)
	done := func() { next <- struct{}{} }
	tests := []struct {
		action string
		data   string

		// recvMsg callback to use to check the executor state.
		// The function should call t.Error instead of t.Fatal.
		// After the test is done, this must send on the next chan exactly once.
		recvMsg func(action, data string)
	}{{
		action: actionStop,
	}, {
		action: actionFormat,
		data:   `package main;import "fmt"; func main() { fmt.Println("Hello, world!") }`,
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Formatting source...\n"},
			{actionFormat, "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"Hello, world!\") }\n"},
			{clearOutput, ""},
			{statusUpdate, "Source formatted.\n"},
			{statusStopped, ""},
		}),
	}, {
		action: actionFormat,
		data:   "package main\n\n\nnot valid go",
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Formatting source...\n"},
			{appendStderr, "RE> main.go:4:1:.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[4]"},
			{statusStopped, ""},
		}),
	}, {
		action: actionRun,
		data:   "package main\n\n\nnot valid go",
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{appendStderr, "RE> main_test.go:4:1:.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[4]"},
			{statusStopped, ""},
		}),
	}, {
		action: actionRun,
		data:   `package main; import "fmt"; import "os"; func main() { fmt.Fprintln(os.Stderr, "stderr") }`,
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStderr, "stderr\n"},
			{statusUpdate, "Program exited.\n"},
			{statusStopped, ""},
		}),
	}, {
		action: actionRun,
		data:   `package foo; func main(){}`,
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Program must be in 'package main'.\n"},
			{statusStopped, ""},
		}),
	}, {
		action: actionRun,
		data:   `package main; func Main(){}`,
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Program must have either a main function or a set of test functions.\n"},
			{statusStopped, ""},
		}),
	}, {
		action: actionRun,
		data:   `package main; import "testing"; func Test(t *testing.T){t.Error("test error")}`,
		recvMsg: wantMessages(t, done, []message{
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
		action: actionRun,
		data:   `package main; import "time"; func main() { time.Sleep(time.Hour) }`,
		recvMsg: wantMessages(t, done, []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
		}),
	}, {
		action: actionStop,
		recvMsg: wantMessages(t, done, []message{
			{statusUpdate, "RE> Unexpected error:.*\n"},
			{statusUpdate, "\n"},
			{statusStopped, ""},
		}),
	}}

	for i, tt := range tests {
		mu.Lock()
		sendMsg = tt.recvMsg
		if tt.recvMsg == nil {
			sendMsg = func(_, _ string) {} // Avoid nil panic
		}
		mu.Unlock()

		t.Logf("starting test %d, action: %s, data: %q", i, tt.action, tt.data)
		switch tt.action {
		case actionFormat, actionRun:
			ex.Start(tt.action, tt.data)
		case actionStop:
			ex.Stop()
		default:
			t.Fatalf("test %d, unknown action: %s", i, tt.action)
		}

		// Wait until this test is done.
		if tt.recvMsg != nil {
			select {
			case <-next:
				if t.Failed() {
					t.Fatalf("test %d, failed test", i)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("test %d: timed out", i)
			}
		}
	}

	// Test resource cleanup.
	ex.Close()
	if _, err := os.Stat(ex.tmpDir); err == nil {
		t.Errorf("unexpected Stat(%q) success", ex.tmpDir)
	}
}

func wantMessages(t *testing.T, done func(), want []message) func(action, data string) {
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
				t.Errorf("mismatching messages:\ngot  %q\nwant %q", got, want)
			}
			done()
		}
		if len(got) > len(want) {
			t.Errorf("got unexpected message{action: %s, data: %q}", action, data)
		}

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
