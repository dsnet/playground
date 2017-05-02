// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asdine/storm"
)

const (
	boltFile    = "snippets.boltdb"
	defaultID   = 1
	defaultName = "Default snippet"
	defaultCode = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, 世界\")\n}\n"
)

var (
	zeroTime = time.Time{} // Use instead of IsZero since we want exact zero for struct
	maxTime  = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	maxID    = int64(math.MaxInt64 / 2) // Divide-by-two to avoid overflow in storm library
)

// requestError is an error type indicating the user provided bad input.
// These errors can be converted to an HTTP status 400 code.
type requestError struct{ error }

// errNotFound indicates that the error was not found.
// This error can be converted to an HTTP status 404 code.
var errNotFound = storm.ErrNotFound

type snippet struct {
	ID       int64     `storm:"index,increment"`
	Created  time.Time `storm:"index"`
	Modified time.Time `storm:"index"`
	Name     string
	Code     string
}

type database struct {
	db      *storm.DB
	mu      sync.Mutex       // Protects names
	names   map[int64]string // Mapping from IDs to names
	timeNow func() time.Time
}

func openDatabase(path string) (*database, error) {
	// Open the BoltDB file.
	var once sync.Once
	db, err := storm.Open(filepath.Join(path, boltFile))
	if err != nil {
		return nil, err
	}
	defer once.Do(func() { db.Close() })
	if err := storm.Codec(codec{})(db); err != nil {
		return nil, err
	}

	// Cache all of the snippet names.
	names := make(map[int64]string)
	var ss []snippet
	if err := db.All(&ss); err != nil {
		return nil, err
	}
	for _, s := range ss {
		names[s.ID] = strings.ToLower(s.Name)
	}

	// Create default snippet.
	if len(names) == 0 {
		s := &snippet{Name: defaultName, Code: defaultCode}
		if err := db.Save(s); err != nil {
			return nil, err
		}
		if s.ID != defaultID {
			return nil, errors.New("invalid ID for default snippet")
		}
		names[s.ID] = strings.ToLower(defaultName)
	}

	once.Do(func() {}) // Avoid closing database
	return &database{db: db, names: names, timeNow: time.Now}, nil
}

// QueryByModified returns a list of snippets younger than the last time.
// The list is sorted in descending order by time (and by ID on equal times).
func (db *database) QueryByModified(lastTime time.Time, lastID int64, limit int) ([]snippet, error) {
	if lastTime == zeroTime && lastID == 0 {
		lastTime, lastID = maxTime, maxID // Find everything
	}
	var ss []snippet
	err := db.db.Range("Modified", zeroTime, lastTime, &ss, storm.Limit(limit), storm.Reverse())
	if err == errNotFound {
		err = nil
	}
	after := func(s snippet) bool {
		if s.Modified.Equal(lastTime) {
			return s.ID >= lastID
		}
		return s.Modified.After(lastTime)
	}
	for len(ss) > 0 && after(ss[0]) {
		ss = ss[1:]
	}
	return ss, err
}

// QueryByID returns a list of snippets with IDs greater than the last ID.
// The list is sorted in ascending order by ID.
func (db *database) QueryByID(lastID int64, limit int) ([]snippet, error) {
	var ss []snippet
	err := db.db.Range("ID", lastID+1, maxID, &ss, storm.Limit(limit))
	if err == errNotFound {
		err = nil
	}
	return ss, err
}

// QueryByName returns a list of snippets that match the provided query.
// The most relevant snippets are at the front of the list.
func (db *database) QueryByName(name string, limit int) ([]snippet, error) {
	type queryMatch struct {
		id, n int64
		name  string
	}

	// Convert query into a list of lower-case search tokens.
	qss := strings.Split(strings.ToLower(name), " ")
	qs := qss[:0]
	for _, s := range qss {
		if s != "" {
			qs = append(qs, s)
		}
	}
	if name == "" {
		qs = []string{""} // Find everything
	}

	// Search for all snippets that have a match with the query.
	// Assume that the number of snippets is small enough that this is fast.
	var ms []queryMatch
	db.mu.Lock()
	for id, name := range db.names {
		m := queryMatch{id: id, name: name}
		for _, s := range qs {
			m.n += int64(strings.Count(name, s))
		}
		if m.n > 0 {
			ms = append(ms, m)
		}
	}
	db.mu.Unlock()

	// Sort by ranking and apply limit.
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].n == ms[j].n {
			if ms[i].name == ms[j].name {
				return ms[i].id > ms[j].id
			}
			return ms[i].name < ms[j].name
		}
		return ms[i].n > ms[j].n
	})
	if len(ms) > limit && limit >= 0 {
		ms = ms[:limit]
	}

	// Retrieve all snippets for the remaining IDs.
	var ss []snippet
	for _, m := range ms {
		var s snippet
		if err := db.db.One("ID", m.id, &s); err == storm.ErrNotFound {
			continue
		} else if err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	return ss, nil
}

// Create a new snippet. The ID must not be set and the name must not be empty.
// If successful, this will return the ID of the new snippet.
func (db *database) Create(s snippet) (int64, error) {
	switch {
	case s.Name == "":
		return 0, requestError{errors.New("snippet name cannot be empty")}
	case s.ID != 0:
		return 0, requestError{errors.New("cannot assign ID when creating snippet")}
	}
	s.Created = db.timeNow().UTC().AddDate(0, 0, 0)
	s.Modified = s.Created
	if err := db.db.Save(&s); err != nil {
		return 0, err
	}
	db.mu.Lock()
	db.names[s.ID] = strings.ToLower(s.Name)
	db.mu.Unlock()
	return s.ID, nil
}

// Retrieves a snippet by the specified ID.
// If the snippet does not exist, this returns errNotFound.
func (db *database) Retrieve(id int64) (snippet, error) {
	var s snippet
	err := db.db.One("ID", id, &s)
	return s, err
}

// Update updates the provided snippet at the given ID.
// Only the Name and Code of a snippet may be changed.
// If the snippet does not exist, this returns errNotFound.
func (db *database) Update(s snippet, id int64) error {
	switch {
	case s.ID == 0 && id == 0:
		return requestError{errors.New("cannot update snippet with ID: 0")}
	case s.ID > 0 && s.ID != id:
		return requestError{fmt.Errorf("snippet IDs do not match: %d != %d", id, s.ID)}
	case s.ID == defaultID && s.Name != "" && s.Name != defaultName:
		return requestError{errors.New("cannot change default snippet name")}
	case s.Modified != zeroTime || s.Created != zeroTime:
		return requestError{errors.New("cannot set modified or created times")}
	}
	s.ID = id
	s.Modified = db.timeNow().UTC().AddDate(0, 0, 0)
	if err := db.db.Update(&s); err != nil {
		return err
	}
	if s.Name != "" {
		db.mu.Lock()
		db.names[s.ID] = strings.ToLower(s.Name)
		db.mu.Unlock()
	}
	return nil
}

// Delete deletes a snippet by the provided ID.
// If the snippet does not exist, this returns errNotFound.
// The default snippet cannot be deleted.
func (db *database) Delete(id int64) error {
	if id == 0 || id == defaultID {
		return requestError{fmt.Errorf("cannot delete snippet (ID: %d)", id)}
	}
	if err := db.db.DeleteStruct(&snippet{ID: id}); err != nil {
		return err
	}
	db.mu.Lock()
	delete(db.names, id)
	db.mu.Unlock()
	return nil
}

func (db *database) Close() error {
	return db.db.Close()
}

type codec struct{}

func (codec) Marshal(v interface{}) ([]byte, error) {
	switch v := v.(type) {
	case time.Time:
		return v.MarshalBinary()
	case *snippet:
		return json.Marshal(v)
	}
	return nil, fmt.Errorf("unknown type: %T", v)
}
func (codec) Unmarshal(b []byte, v interface{}) error {
	switch v := v.(type) {
	case *time.Time:
		return v.UnmarshalBinary(b)
	case *snippet:
		return json.Unmarshal(b, v)
	}
	return fmt.Errorf("unknown type: %T", v)
}
func (codec) Name() string {
	return ""
}
