// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boltdb/bolt"
)

const (
	boltFile     = "snippets.boltdb"
	bucketByID   = "SnippetsByID"
	bucketByDate = "SnippetsByModified"

	defaultID   = 1
	defaultName = "Default snippet"
	defaultCode = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, 世界\")\n}\n"
)

var (
	maxTime = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	maxID   = int64(math.MaxInt64)
)

// requestError is an error type indicating the user provided bad input.
// These errors can be converted to an HTTP status 400 code.
type requestError struct{ error }

// errNotFound indicates that the error was not found.
// This error can be converted to an HTTP status 404 code.
var errNotFound = errors.New("not found")

type snippet struct {
	// These fields are only updated by the database.
	ID       int64     `json:"id"`
	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`

	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

func (s *snippet) MarshalBinary() ([]byte, error) {
	type st snippet
	bb := new(bytes.Buffer)
	enc := gob.NewEncoder(bb)
	err := enc.Encode((*st)(s))
	return bb.Bytes(), err
}

func (s *snippet) UnmarshalBinary(b []byte) error {
	type st snippet
	br := bytes.NewReader(b)
	dec := gob.NewDecoder(br)
	return dec.Decode((*st)(s))
}

func idKey(id int64) []byte {
	// Offset the int64 values sort that they sort nicely as uint64.
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], uint64(id+math.MaxInt64+1))
	return k[:]
}

func dualKey(id int64, mod time.Time) []byte {
	// Offset the int64 values sort that they sort nicely as uint64.
	var k [20]byte
	binary.BigEndian.PutUint64(k[:8], uint64(mod.Unix()+math.MaxInt64+1))
	binary.BigEndian.PutUint32(k[8:12], uint32(mod.Nanosecond()))
	binary.BigEndian.PutUint64(k[12:], uint64(id+math.MaxInt64+1))
	return k[:]
}

type database struct {
	db     *bolt.DB
	lastID int64

	mu      sync.Mutex // Protects names
	names   map[int64]string
	timeNow func() time.Time
}

func openDatabase(path string) (*database, error) {
	// Open the BoltDB file.
	var once sync.Once
	db, err := bolt.Open(filepath.Join(path, boltFile), 0644, nil)
	if err != nil {
		return nil, err
	}
	defer once.Do(func() { db.Close() })

	// Get the last snippet ID and all names.
	lastID := int64(-1)
	names := make(map[int64]string)
	if err := db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(bucketByID))
		if bkt == nil {
			return nil
		}
		c := bkt.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var s snippet
			if err := s.UnmarshalBinary(v); err != nil {
				return err
			}
			names[s.ID] = strings.ToLower(s.Name)
			lastID = s.ID
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Create default snippet.
	if lastID == -1 {
		s := snippet{ID: defaultID, Name: defaultName, Code: defaultCode}
		err := db.Update(func(tx *bolt.Tx) error {
			bktByID, err := tx.CreateBucketIfNotExists([]byte(bucketByID))
			if err != nil {
				return err
			}
			bktByDate, err := tx.CreateBucketIfNotExists([]byte(bucketByDate))
			if err != nil {
				return err
			}

			v, _ := s.MarshalBinary()
			if err := bktByID.Put(idKey(s.ID), v); err != nil {
				return err
			}
			if err := bktByDate.Put(dualKey(s.ID, s.Modified), nil); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		lastID = s.ID
	}

	once.Do(func() {}) // Avoid closing database
	return &database{db: db, lastID: lastID, names: names, timeNow: time.Now}, nil
}

// QueryByModified returns a list of snippets younger than the last time.
// The list is sorted in descending order by time (and by ID on equal times).
func (db *database) QueryByModified(lastTime time.Time, lastID int64, limit int) ([]snippet, error) {
	if lastTime.IsZero() && lastID == 0 {
		lastTime, lastID = maxTime, maxID // Find everything
	}
	var ss []snippet
	err := db.db.View(func(tx *bolt.Tx) error {
		// Seek to the latest value that is immediately before the search key.
		bktByDate := tx.Bucket([]byte(bucketByDate))
		c := bktByDate.Cursor()
		sk := dualKey(lastID, lastTime)
		k, _ := c.Seek(sk)
		if k == nil {
			k, _ = c.Last()
		}

		// Iterate through all results.
		ss = nil
		bktByID := tx.Bucket([]byte(bucketByID))
		for ; k != nil; k, _ = c.Prev() {
			if len(ss) >= limit && limit >= 0 {
				break
			}
			if bytes.Compare(k, sk) >= 0 {
				continue
			}
			var s snippet
			v := bktByID.Get(k[12:20]) // Extract ID from dual key
			if err := s.UnmarshalBinary(v); err != nil {
				return err
			}
			ss = append(ss, s)
		}
		return nil
	})
	return ss, err
}

// QueryByID returns a list of snippets with IDs greater than the last ID.
// The list is sorted in ascending order by ID.
func (db *database) QueryByID(lastID int64, limit int) ([]snippet, error) {
	var ss []snippet
	err := db.db.View(func(tx *bolt.Tx) error {
		// Iterate through all results.
		ss = nil
		bktByID := tx.Bucket([]byte(bucketByID))
		c := bktByID.Cursor()
		sk := idKey(lastID + 1)
		for k, v := c.Seek(sk); k != nil; k, v = c.Next() {
			if len(ss) >= limit && limit >= 0 {
				break
			}
			var s snippet
			if err := s.UnmarshalBinary(v); err != nil {
				return err
			}
			ss = append(ss, s)
		}
		return nil
	})
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
	for len(ms) > limit && limit >= 0 {
		ms = ms[:limit]
	}

	// Retrieve all snippets for the remaining IDs.
	var ss []snippet
	for _, m := range ms {
		s, err := db.Retrieve(m.id)
		if err == errNotFound {
			continue
		}
		if err != nil {
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
	s.ID = atomic.AddInt64(&db.lastID, 1)
	err := db.db.Update(func(tx *bolt.Tx) error {
		s.Created = db.timeNow().UTC().AddDate(0, 0, 0)
		s.Modified = s.Created

		// Store the snippet.
		v, _ := s.MarshalBinary()
		bktByID := tx.Bucket([]byte(bucketByID))
		if err := bktByID.Put(idKey(s.ID), v); err != nil {
			return err
		}
		bktByDate := tx.Bucket([]byte(bucketByDate))
		if err := bktByDate.Put(dualKey(s.ID, s.Modified), nil); err != nil {
			return err
		}
		return nil
	})
	if s.ID > 0 && err == nil {
		db.mu.Lock()
		db.names[s.ID] = strings.ToLower(s.Name)
		db.mu.Unlock()
	}
	return s.ID, err
}

// Retrieves a snippet by the specified ID.
// If the snippet does not exist, this returns errNotFound.
func (db *database) Retrieve(id int64) (snippet, error) {
	var s snippet
	err := db.db.View(func(tx *bolt.Tx) error {
		bktByID := tx.Bucket([]byte(bucketByID))
		v := bktByID.Get(idKey(id))
		if v == nil {
			return errNotFound
		}
		return s.UnmarshalBinary(v)
	})
	return s, err
}

// Update updates the provided snippet at the given ID.
// The ID field in the snippet is optional as long as id is valid.
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
	case !s.Modified.IsZero() || !s.Created.IsZero():
		return requestError{errors.New("cannot set modified or created times")}
	}
	err := db.db.Update(func(tx *bolt.Tx) error {
		// Locate the snippet associated with s.ID.
		bktByID := tx.Bucket([]byte(bucketByID))
		v := bktByID.Get(idKey(id))
		if v == nil {
			return errNotFound
		}
		var s2 snippet
		if err := s2.UnmarshalBinary(v); err != nil {
			return err
		}

		// Update bucketsByID with the new value.
		if s.Name != "" {
			s2.Name = s.Name
		}
		if s.Code != "" {
			s2.Code = s.Code
		}
		oldKey := dualKey(s2.ID, s2.Modified)
		s2.Modified = db.timeNow().UTC().AddDate(0, 0, 0)
		newKey := dualKey(s2.ID, s2.Modified)
		v2, err := s2.MarshalBinary()
		if err != nil {
			return err
		}
		if err := bktByID.Put(idKey(id), v2); err != nil {
			return err
		}

		// Update bucketsByDate.
		bktByDate := tx.Bucket([]byte(bucketByDate))
		if err := bktByDate.Delete(oldKey); err != nil {
			return err
		}
		return bktByDate.Put(newKey, nil)
	})
	if id > 0 && s.Name != "" && err == nil {
		db.mu.Lock()
		db.names[id] = strings.ToLower(s.Name)
		db.mu.Unlock()
	}
	return err
}

// Delete deletes a snippet by the provided ID.
// If the snippet does not exist, this returns errNotFound.
// The default snippet cannot be deleted.
func (db *database) Delete(id int64) error {
	if id == 0 || id == defaultID {
		return requestError{fmt.Errorf("cannot delete snippet (ID: %d)", id)}
	}
	err := db.db.Update(func(tx *bolt.Tx) error {
		// Locate and delete key from bucketsByID.
		bktByID := tx.Bucket([]byte(bucketByID))
		v := bktByID.Get(idKey(id))
		if v == nil {
			return errNotFound
		}
		if err := bktByID.Delete(idKey(id)); err != nil {
			return err
		}

		// Delete key from bucketsByDate.
		var s snippet
		if err := s.UnmarshalBinary(v); err != nil {
			return err
		}
		k := dualKey(s.ID, s.Modified)
		return tx.Bucket([]byte(bucketByDate)).Delete(k)
	})
	if err == nil {
		db.mu.Lock()
		delete(db.names, id)
		db.mu.Unlock()
	}
	return err
}

func (db *database) Close() error {
	return db.db.Close()
}
