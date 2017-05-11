// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testLogger struct{ *testing.T }

func (t testLogger) Printf(f string, x ...interface{}) { t.Logf(f, x...) }

func TestPlayground(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	pwSalt := sha256.Sum256([]byte("salt"))
	pwHash := sha256.Sum256(append(pwSalt[:], "pass"...))

	// Create a new playground HTTP handler.
	pg, err := newPlayground(pwHash, pwSalt, tmpDir, "go", "gofmt", nil, testLogger{t})
	if err != nil {
		t.Fatalf("newPlayground error: %v", err)
	}
	defer pg.Close()

	// Listen to TCP on some ephemeral port.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("net.Listen error: %v", err)
	}
	defer ln.Close()

	// Start a HTTP server to handle traffic on that socket.
	srv := &http.Server{Handler: pg}
	go func() { srv.Serve(ln) }()
	defer srv.Close()

	mt := newMessageTester(t)
	jar, _ := cookiejar.New(nil)
	cln := &http.Client{Jar: jar}

	bodyChecker := func(wantType string, wantBody []byte) func(string, []byte) {
		return func(gotType string, gotBody []byte) {
			if gotType != wantType {
				mt.Errorf("Content-Type mismatch: got %q, want %q", gotType, wantType)
			}
			if !bytes.Equal(gotBody, wantBody) {
				mt.Errorf("body data mismatch: got %d bytes, want %d bytes", len(gotBody), len(wantBody))
			}
		}
	}
	snippetChecker := func(want snippet) func(string, []byte) {
		return func(gotType string, gotBody []byte) {
			if wantType := "application/json"; gotType != wantType {
				mt.Errorf("Content-Type mismatch: got %q, want %q", gotType, wantType)
			}
			var got snippet
			if err := json.Unmarshal(gotBody, &got); err != nil {
				mt.Errorf("json.Unmarshal error: %v", err)
			}
			got.Created, got.Modified = time.Time{}, time.Time{}
			if got != want {
				mt.Errorf("mismatching snippet: got %v, want %v", got, want)
			}
		}
	}
	snippetsChecker := func(want []snippet) func(string, []byte) {
		return func(gotType string, gotBody []byte) {
			if wantType := "application/json"; gotType != wantType {
				mt.Errorf("Content-Type mismatch: got %q, want %q", gotType, wantType)
			}
			var got []snippet
			if err := json.Unmarshal(gotBody, &got); err != nil {
				mt.Errorf("json.Unmarshal error: %v", err)
			}
			for i := range got {
				got[i].Created, got[i].Modified = time.Time{}, time.Time{}
			}
			if !reflect.DeepEqual(got, want) {
				mt.Errorf("mismatching snippets:\ngot  %v\nwant %v", got, want)
			}
		}
	}

	httpTests := []struct {
		label string

		url    string
		method string
		ctype  string
		body   []byte

		wantStatus int
		checkBody  func(string, []byte)
	}{{
		label:      "GetFavicon",
		url:        "/favicon.ico",
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody:  bodyChecker(mimeTypes["ico"], staticFS["img/favicon.ico"]),
	}, {
		label:      "GetRootLogin",
		url:        "/1",
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody:  bodyChecker(mimeTypes["html"], staticFS["html/playground-login.html"]),
	}, {
		label:      "UnauthorizedSnippets",
		url:        "/snippets",
		method:     "GET",
		wantStatus: http.StatusUnauthorized,
	}, {
		label:      "AuthenticateReject",
		url:        "/login",
		method:     "POST",
		ctype:      "application/octet-stream",
		body:       []byte("bad password"),
		wantStatus: http.StatusUnauthorized,
	}, {
		label:      "AuthenticateAllow",
		url:        "/login",
		method:     "POST",
		ctype:      "application/octet-stream",
		body:       []byte("pass"),
		wantStatus: http.StatusOK,
	}, {
		label:      "GetRootPlayground",
		url:        "/1",
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody:  bodyChecker(mimeTypes["html"], staticFS["html/playground.html"]),
	}, {
		label:      "GetDefaultSnippet",
		url:        fmt.Sprintf("/snippets/%d", defaultID),
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody:  snippetChecker(snippet{ID: defaultID, Name: defaultName, Code: defaultCode}),
	}, {
		label:      "GetNotFound1",
		url:        fmt.Sprintf("/snippets/%d", defaultID+1),
		method:     "GET",
		wantStatus: http.StatusNotFound,
	}, {
		label:      "CreateSnippet1",
		url:        "/snippets",
		method:     "POST",
		ctype:      "application/json",
		body:       []byte(fmt.Sprintf(`{"Name": "snippet%d", "Code": "code%d"}`, defaultID+1, defaultID+1)),
		wantStatus: http.StatusOK,
		checkBody: snippetChecker(snippet{
			ID:   defaultID + 1,
			Name: fmt.Sprintf("snippet%d", defaultID+1),
			Code: fmt.Sprintf("code%d", defaultID+1),
		}),
	}, {
		label:      "CreateSnippet2",
		url:        "/snippets",
		method:     "POST",
		ctype:      "application/json",
		body:       []byte(fmt.Sprintf(`{"Name": "snippet%d", "Code": "code%d"}`, defaultID+2, defaultID+2)),
		wantStatus: http.StatusOK,
		checkBody: snippetChecker(snippet{
			ID:   defaultID + 2,
			Name: fmt.Sprintf("snippet%d", defaultID+2),
			Code: fmt.Sprintf("code%d", defaultID+2),
		}),
	}, {
		label:      "CreateSnippet3",
		url:        "/snippets",
		method:     "POST",
		ctype:      "application/json",
		body:       []byte(fmt.Sprintf(`{"Name": "snippet%d", "Code": "code%d"}`, defaultID+3, defaultID+3)),
		wantStatus: http.StatusOK,
		checkBody: snippetChecker(snippet{
			ID:   defaultID + 3,
			Name: fmt.Sprintf("snippet%d", defaultID+3),
			Code: fmt.Sprintf("code%d", defaultID+3),
		}),
	}, {
		label:      "GetSnippet1",
		url:        fmt.Sprintf("/snippets/%d", defaultID+2),
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody: snippetChecker(snippet{
			ID:   defaultID + 2,
			Name: fmt.Sprintf("snippet%d", defaultID+2),
			Code: fmt.Sprintf("code%d", defaultID+2),
		}),
	}, {
		label:      "PutInvalidJSON",
		url:        fmt.Sprintf("/snippets/%d", defaultID+2),
		method:     "PUT",
		ctype:      "application/json",
		body:       []byte("bad JSON"),
		wantStatus: http.StatusBadRequest,
	}, {
		label:      "PutValidJSON",
		url:        fmt.Sprintf("/snippets/%d", defaultID+2),
		method:     "PUT",
		ctype:      "application/json",
		body:       []byte(fmt.Sprintf(`{"Name": "snippet%d", "Code": "code%da"}`, defaultID+2, defaultID+2)),
		wantStatus: http.StatusOK,
	}, {
		label:      "GetSnippet2",
		url:        fmt.Sprintf("/snippets/%d", defaultID+2),
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody: snippetChecker(snippet{
			ID:   defaultID + 2,
			Name: fmt.Sprintf("snippet%d", defaultID+2),
			Code: fmt.Sprintf("code%da", defaultID+2),
		}),
	}, {
		label:      "DeleteNotFound",
		url:        fmt.Sprintf("/snippets/%d", defaultID+500),
		method:     "DELETE",
		wantStatus: http.StatusNotFound,
	}, {
		label:      "DeleteSnippet",
		url:        fmt.Sprintf("/snippets/%d", defaultID+1),
		method:     "DELETE",
		wantStatus: http.StatusOK,
	}, {
		label:      "GetNotFound2",
		url:        fmt.Sprintf("/snippets/%d", defaultID+1),
		method:     "GET",
		wantStatus: http.StatusNotFound,
	}, {
		label:      "QueryByID",
		url:        fmt.Sprintf(`/snippets?query={"ID":%d}`, defaultID+1),
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody: snippetsChecker([]snippet{
			{ID: defaultID + 2, Name: fmt.Sprintf("snippet%d", defaultID+2)},
			{ID: defaultID + 3, Name: fmt.Sprintf("snippet%d", defaultID+3)},
		}),
	}, {
		label:      "QueryByName",
		url:        `/snippets?query={"Name":"snippet"}&queryBy=name&limit=2`,
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody: snippetsChecker([]snippet{
			{ID: defaultID + 2, Name: "snippet3"},
			{ID: defaultID + 3, Name: "snippet4"},
		}),
	}, {
		label: "QueryByModified",
		url: func() string {
			js, _ := json.Marshal(struct{ Modified time.Time }{time.Now().Add(time.Hour)})
			return fmt.Sprintf(`/snippets?query=%s&queryBy=modified&allFields=true`, js)
		}(),
		method:     "GET",
		wantStatus: http.StatusOK,
		checkBody: snippetsChecker([]snippet{
			{ID: defaultID + 2, Name: "snippet3", Code: "code3a"},
			{ID: defaultID + 3, Name: "snippet4", Code: "code4"},
			{ID: defaultID, Name: "Default snippet", Code: defaultCode},
		}),
	}}

	for _, tt := range httpTests {
		t.Run(tt.label, func(t *testing.T) {
			mt.SetT(t)

			var r io.Reader
			if tt.body != nil {
				r = bytes.NewReader(tt.body)
			}
			req, err := http.NewRequest(tt.method, fmt.Sprintf("http://%v%s", ln.Addr(), tt.url), r)
			if err != nil {
				t.Fatalf("http.NewRequest error: %v", err)
			}
			if tt.ctype != "" {
				req.Header.Set("Content-Type", tt.ctype)
			}
			resp, err := cln.Do(req)
			if err != nil {
				t.Fatalf("client.Do error: %v", err)
			}
			defer resp.Body.Close()
			if got := resp.StatusCode; got != tt.wantStatus {
				t.Fatalf("Response.StatusCode = %d, want %d", got, tt.wantStatus)
			}
			if tt.checkBody != nil {
				body, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("Body.Read error: %v", err)
				}
				tt.checkBody(resp.Header.Get("Content-Type"), body)
			}
		})
		if t.Failed() {
			return
		}
	}

	// Ensure that websockets require authentication as well.
	dl := websocket.Dialer{}
	_, _, err = dl.Dial(fmt.Sprintf("ws://%v/websocket", ln.Addr()), nil)
	if err == nil {
		t.Fatalf("unexpected websocket.Dial success")
	}

	var done int32
	dl = websocket.Dialer{Jar: jar} // Inherit authentication tokens
	conn, _, err := dl.Dial(fmt.Sprintf("ws://%v/websocket", ln.Addr()), nil)
	if err != nil {
		t.Fatalf("websocket.Dial error: %v", err)
	}
	defer conn.Close()
	defer atomic.StoreInt32(&done, 1)

	// Message reader loop.
	go func() {
		for {
			typ, b, err := conn.ReadMessage()
			if err != nil {
				if atomic.LoadInt32(&done) != 1 {
					mt.Errorf("conn.ReadMessage error: %v", err)
				}
				return
			}
			if typ != websocket.TextMessage {
				mt.Errorf("unexpected message type: got %d, want %d", typ, websocket.TextMessage)
				return
			}
			var m map[string]string
			if err := json.Unmarshal(b, &m); err != nil {
				mt.Errorf("json.Unmarshal error: %v", err)
				return
			}
			mt.SendMessage(m["action"], m["data"])
		}
	}()

	var (
		mu    sync.Mutex
		blobs = make(map[string]string) // Key: ID, Value: Name
	)
	websocketTests := []struct {
		label string // Name of the test
		long  bool   // Does this test take a long time?

		action string
		data   string

		// Either want or check will be set.
		want  []message                 // List of expected messages
		check func(action, data string) // Callback function to check each message
	}{{
		label:  "ClearOutput",
		action: clearOutput,
		want: []message{
			{clearOutput, ""},
		},
	}, {
		label:  "RunInvalid",
		action: actionRun,
		data:   "package main\n\nfunc main(){invalid}",
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{appendStderr, "RE> main.go:3.*\n"},
			{statusUpdate, "RE> Unexpected error: .*\n"},
			{markLines, "[3]"},
			{statusStopped, ""},
		},
	}, {
		label:  "RunValid",
		action: actionRun,
		data:   `package main; import "fmt"; func main() {fmt.Println("Hello")}`,
		want: []message{
			{statusStarted, ""},
			{clearOutput, ""},
			{statusUpdate, "Compiling program...\n"},
			{clearOutput, ""},
			{appendStdout, "Hello\n"},
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
						var m map[string]string
						if err := json.Unmarshal([]byte(data), &m); err != nil {
							mt.Errorf("json.Unmarshal error: %v", err)
						}
						mu.Lock()
						blobs[m["id"]] = m["name"]
						mu.Unlock()
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

	for _, tt := range websocketTests {
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

			b, _ := json.Marshal(map[string]string{"action": tt.action, "data": tt.data})
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				t.Fatalf("conn.WriteMessage error: %v", err)
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
		if t.Failed() {
			return
		}
	}

	// Check that we can retrieve all blobs.
	for id, name := range blobs {
		resp, err := cln.Get(fmt.Sprintf("http://%v/dynamic/%s", ln.Addr(), id))
		if err != nil {
			t.Fatalf("Client.Get error: %v", err)
		}
		defer resp.Body.Close()
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("Response.StatusCode = %d, want %d", got, want)
		}
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Body.Read error: %v", err)
		}
		if got, want := resp.Header.Get("Content-Type"), mimeFromPath(name); got != want {
			t.Fatalf("Content-Type mismatch: got %q, want %q", got, want)
		}
		if len(body) == 0 {
			t.Fatalf("unexpected empty profile")
		}
	}
}

func TestAuthToken(t *testing.T) {
	pw1 := sha256.Sum256([]byte("password1"))
	pw2 := sha256.Sum256([]byte("password2"))

	now := time.Now().UTC()
	s := formatAuthToken(pw1[:], now)
	if got := parseAuthToken(pw1[:], s); !now.Equal(got) {
		t.Error("parseAuthToken: got %v, want %v", got, now)
	}
	if got := parseAuthToken(pw2[:], s); now.Equal(got) {
		t.Error("unexpected parseAuthToken success with bad password")
	}
}
