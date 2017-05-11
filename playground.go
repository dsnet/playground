// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

//go:generate go run staticfs_gen.go

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type logger interface {
	Printf(string, ...interface{})
}

type playground struct {
	// Password values used to authenticate each HTTP request.
	pwHash [sha256.Size]byte // Must be SHA256(pwSalt+password)
	pwSalt [sha256.Size]byte

	// Arguments to the code executor.
	gcBin  string
	fmtBin string
	gcBins map[string]string

	bs  *blobStore
	sdb *database
	log logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// clientID and numActive are atomically incremented by serveWebsocket.
	clientID  int64 // Some unique ID number for connections
	numActive int64 // Number of currently active connections
}

func newPlayground(pwHash, pwSalt [sha256.Size]byte, dbPath, gcBin, fmtBin string, gcBins map[string]string, log logger) (*playground, error) {
	db, err := openDatabase(dbPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &playground{
		pwHash: pwHash,
		pwSalt: pwSalt,
		gcBin:  gcBin,
		fmtBin: fmtBin,
		gcBins: gcBins,

		bs:  newBlobStore(),
		sdb: db,
		log: log,

		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func (pg *playground) Close() error {
	pg.cancel()
	pg.wg.Wait()
	return pg.sdb.Close()
}

var (
	reStatic     = regexp.MustCompile(`^/static/`)
	reLogin      = regexp.MustCompile(`^/login$`)
	reRoot       = regexp.MustCompile(`^/[0-9]*$`)
	reSnippets   = regexp.MustCompile(`^/snippets$`)
	reSnippetsID = regexp.MustCompile(`^/snippets/[0-9]+$`)
	reWebsocket  = regexp.MustCompile(`^/websocket$`)
	reDynamic    = regexp.MustCompile(`^/dynamic/[-_a-zA-Z0-9]+$`)
)

func (pg *playground) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pg.wg.Add(1)
	defer pg.wg.Done()

	select {
	case <-pg.ctx.Done():
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	default:
	}

	if r.URL.Path == "/favicon.ico" {
		r.URL.Path = "/static/img/favicon.ico" // Server-side redirect
	}
	switch {
	case matchRequest(r, reStatic, "GET"):
		// Static content is always available without authentication.
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/static")
		pg.serveStatic(w, r)
		return
	case !pg.isAuthenticated(w, r) || reLogin.MatchString(r.URL.Path):
		// Perform authentication check prior to serving any other content.
		pg.serveLogin(w, r)
		return
	case matchRequest(r, reRoot, "GET"):
		r.URL.Path = "/html/playground.html"
		pg.serveStatic(w, r)
		return
	case matchRequest(r, reSnippets, "GET"):
		pg.serveListing(w, r)
		return
	case matchRequest(r, reSnippets, "POST") ||
		matchRequest(r, reSnippetsID, "GET", "PUT", "DELETE"):
		pg.serveSnippet(w, r)
		return
	case matchRequest(r, reWebsocket, "GET", "CONNECT"):
		pg.serveWebsocket(w, r)
		return
	case matchRequest(r, reDynamic, "GET"):
		pg.serveDynamic(w, r)
		return
	default:
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}
}

func matchRequest(r *http.Request, re *regexp.Regexp, methods ...string) bool {
	if !re.MatchString(r.URL.Path) {
		return false
	}
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	return false
}

const (
	authRefreshPeriod = 1 * 24 * time.Hour // 1 day
	authExpirePeriod  = 7 * 24 * time.Hour // 1 week
)

// formatAuthToken formats the Time as a signed string using HMAC.
func formatAuthToken(key []byte, t time.Time) string {
	bt, _ := t.UTC().MarshalBinary()
	mac := hmac.New(sha256.New, key)
	mac.Write(bt)
	return fmt.Sprintf("%02x%x%x", len(bt), bt, mac.Sum(nil))
}

// parseAuthToken parses and validates an encoded Time.
// If this is an invalid token, then a zero time is returned.
func parseAuthToken(key []byte, s string) time.Time {
	b, err := hex.DecodeString(s)
	if len(b) == 0 || int(b[0]) > len(b[1:]) || err != nil {
		return time.Time{}
	}
	bt, bmac := b[1:1+b[0]], b[1+b[0]:]
	mac := hmac.New(sha256.New, key)
	mac.Write(bt)
	var t time.Time
	err = t.UnmarshalBinary(bt)
	if !hmac.Equal(mac.Sum(nil), bmac) || err != nil {
		return time.Time{}
	}
	return t
}

func (pg *playground) isAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	if pg.pwHash == [sha256.Size]byte{} {
		return true // No password set
	}
	for _, c := range r.Cookies() {
		if c.Name == "auth" {
			t := parseAuthToken(pg.pwHash[:], c.Value)
			if t.IsZero() {
				return false
			}
			d := time.Now().Sub(t)
			if d > authExpirePeriod {
				return false
			}
			if d > authRefreshPeriod {
				pg.refreshAuth(w, r)
			}
			return true
		}
	}
	return false
}

func (pg *playground) refreshAuth(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:    "auth",
		Value:   formatAuthToken(pg.pwHash[:], time.Now()),
		Path:    "/",
		Expires: time.Now().Add(authExpirePeriod),
		MaxAge:  int(authExpirePeriod / time.Second),
		Secure:  r.TLS != nil,
	})
}

func (pg *playground) serveLogin(w http.ResponseWriter, r *http.Request) {
	switch {
	case matchRequest(r, reLogin, "POST"):
		b, _ := ioutil.ReadAll(r.Body)
		if h := sha256.Sum256(append(pg.pwSalt[:], b...)); h == pg.pwHash {
			pg.refreshAuth(w, r)
			w.WriteHeader(http.StatusOK)
			pg.log.Printf("authentication success for client at %s", r.RemoteAddr)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		pg.log.Printf("authentication failure for client at %s", r.RemoteAddr)
		return
	case matchRequest(r, reLogin, "GET") ||
		matchRequest(r, reRoot, "GET"):
		r.URL.Path = "/html/playground-login.html"
		pg.serveStatic(w, r)
		return
	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
}

// serveListing provides an endpoint to return information about snippets.
//
// The endpoint supports several URL query parameters:
//
//	* query: string - The query value to use. This a JSON representation of
//		a snippet. The fields that matter is dependent on the queryBy mode.
//	* queryBy: string - Determines the type of query to perform
//		(must be of "id", "modified", of "name") and defaults to "id".
//	* limit: int - Determines the maximum number of snippet records to return.
//		Default value is 100.
//	* allFields: bool - Controls whether all snippets fields are shown.
//		Default is false; which means, the "code" field will be absent.
//
// To get a JSON dump of all snippets, use the following query:
//	?queryBy=id&limit=-1&allFields=true
func (pg *playground) serveListing(w http.ResponseWriter, r *http.Request) {
	// Parse out the query parameters.
	var query snippet
	queryBy := "id"
	limit := 100
	allFields := false
	for k, v := range r.URL.Query() {
		var err error
		switch k {
		case "query":
			err = json.Unmarshal([]byte(v[0]), &query)
		case "queryBy":
			queryBy = v[0]
			if queryBy != "modified" && queryBy != "id" && queryBy != "name" {
				err = fmt.Errorf("invalid queryBy value: %v", queryBy)
			}
		case "limit":
			limit, err = strconv.Atoi(v[0])
		case "allFields":
			allFields, err = strconv.ParseBool(v[0])
		default:
			err = fmt.Errorf("unknown query field: %v", k)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Perform the query operation upon the snippet database.
	var ss []snippet
	var err error
	switch queryBy {
	case "modified":
		ss, err = pg.sdb.QueryByModified(query.Modified, query.ID, limit)
	case "id":
		ss, err = pg.sdb.QueryByID(query.ID, limit)
	case "name":
		ss, err = pg.sdb.QueryByName(query.Name, limit)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Apply fields filter.
	if !allFields {
		for i := range ss {
			ss[i].Code = ""
		}
	}

	// Compose and write the JSON snippets.
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(ss)
	w.Write(b)
}

// serveSnippet provides an endpoint to perform CRUD operations on a snippet.
func (pg *playground) serveSnippet(w http.ResponseWriter, r *http.Request) {
	var err error

	// Parse out the ID.
	var id int64
	if r.Method != "POST" {
		ss := strings.Split(r.URL.Path, "/")
		id, err = strconv.ParseInt(ss[len(ss)-1], 10, 64)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Read and parse the JSON snippet.
	var s snippet
	if r.Method == "PUT" || r.Method == "POST" {
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(b, &s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Perform the CRUD operation.
	switch r.Method {
	case "POST":
		s.ID, err = pg.sdb.Create(s)
		pg.log.Printf("created snippet %d", s.ID)
	case "GET":
		s, err = pg.sdb.Retrieve(id)
		pg.log.Printf("retrieved snippet %d", id)
	case "PUT":
		err = pg.sdb.Update(s, id)
		pg.log.Printf("updated snippet %d", id)
	case "DELETE":
		err = pg.sdb.Delete(id)
		pg.log.Printf("deleted snippet %d", id)
	}
	if err != nil {
		status := http.StatusInternalServerError
		if _, ok := err.(requestError); ok {
			status = http.StatusBadRequest
		} else if err == errNotFound {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	// Compose and write the JSON snippet.
	if r.Method == "POST" || r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(s)
		w.Write(b)
	}
}

// serveWebsocket provides an endpoint that allows the client to execute
// arbitrary Go code via WebSocket messages.
func (pg *playground) serveWebsocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		pg.log.Printf("unexpected websocket error: %v", err)
		return
	}

	// Allow for cancelation of the connection.
	ctx, cancel := context.WithCancel(pg.ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// Log the websocket for debugging.
	cid := atomic.AddInt64(&pg.clientID, 1)
	pg.log.Printf("websocket client %d at %s connected (%d active)",
		cid, r.RemoteAddr, atomic.AddInt64(&pg.numActive, +1))
	defer func() {
		pg.log.Printf("websocket client %d at %s disconnected (%d active)",
			cid, r.RemoteAddr, atomic.AddInt64(&pg.numActive, -1))
	}()

	// Abstractions of the connection to send JSON messages.
	var m sync.Mutex
	type jsonMessage struct {
		Action string `json:"action"`
		Data   string `json:"data"`
	}
	recvMessage := func() (action, data string, err error) {
		var msg jsonMessage
		_, b, err := conn.ReadMessage()
		json.Unmarshal(b, &msg)
		return msg.Action, msg.Data, err
	}
	sendMessage := func(action, data string) error {
		m.Lock()
		defer m.Unlock()
		b, _ := json.Marshal(jsonMessage{Action: action, Data: data})
		return conn.WriteMessage(websocket.TextMessage, b)
	}

	// Continually accept commands from client until socket closes.
	ex := newExecutor(pg.bs, pg.gcBin, pg.fmtBin, pg.gcBins, sendMessage)
	defer ex.Close()
	for {
		action, data, err := recvMessage()
		if err != nil {
			return // Treat network errors as permanent
		}

		if action != clearOutput {
			pg.log.Printf("%s action by client %d", action, cid)
		}
		switch action {
		case actionRun, actionFormat:
			ex.Start(action, data)
		case actionStop:
			ex.Stop()
		case clearOutput:
			// Client sends this with the expectation that it is echoed back
			// to itself after the server has responded all preceding messages.
			sendMessage(clearOutput, "")
		default:
			ex.sendMsg(statusUpdate, fmt.Sprintf("Unknown action: %v\n", action))
		}
	}
}

func (pg *playground) serveStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimLeft(path.Clean(r.URL.Path), "/")
	b := staticFS[p]
	if b == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mimeFromPath(p))
	w.Write(b)
}

func (pg *playground) serveDynamic(w http.ResponseWriter, r *http.Request) {
	var id string
	if i := strings.LastIndexByte(r.URL.Path, '/'); i >= 0 {
		id = r.URL.Path[i+1:]
	}
	b := pg.bs.Retrieve(id)
	if b.data == nil || b.mime == "" {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", b.mime)
	w.Write(b.data)
}
