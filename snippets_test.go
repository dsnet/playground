// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"io/ioutil"
	"os"
	"testing"
	"time"
)

func equalSnippet(x, y snippet) bool {
	return x.ID == y.ID &&
		x.Created.Equal(y.Created) &&
		x.Modified.Equal(y.Modified) &&
		x.Name == y.Name &&
		x.Code == y.Code
}

func equalSnippets(x, y []snippet) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !equalSnippet(x[i], y[i]) {
			return false
		}
	}
	return true
}

func TestDatabase(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	closer := func() error { return nil }
	defer func() { closer() }()

	// Open the database.
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	db, err := openDatabase(tmpDir)
	if err != nil {
		t.Fatalf("openDatabase error: %v", err)
	}
	closer = db.Close
	db.timeNow = func() time.Time { return now }

	// Types of expected response errors.
	errFuncs := map[string]func(error) bool{
		"IsRequestError": func(err error) bool {
			_, ok := err.(requestError)
			return ok
		},
		"IsNotFound": func(err error) bool {
			return err == errNotFound
		},
		"IsAny": func(err error) bool {
			return err != nil
		},
	}

	type (
		TestCreate struct {
			in snippet
			id int64
		}
		TestRetrieve struct {
			id  int64
			out snippet
		}
		TestUpdate struct {
			in snippet
			id int64
		}
		TestDelete struct {
			id int64
		}
		TestQueryByModified struct {
			modified time.Time
			id       int64
			limit    int
			out      []snippet
		}
		TestQueryByID struct {
			id    int64
			limit int
			out   []snippet
		}
		TestQueryByName struct {
			name  string
			limit int
			out   []snippet
		}
		TestReopen struct{}
	)

	step := 1 * time.Second
	tests := []struct {
		test interface{}   // The specific test to run
		errf string        // Name of callback function to check error
		add  time.Duration // Amount of time to add after each test
	}{{
		TestRetrieve{id: defaultID - 1}, "IsNotFound", step,
	}, {
		TestRetrieve{id: defaultID, out: snippet{
			ID: defaultID, Name: defaultName, Code: defaultCode,
		}}, "", step,
	}, {
		TestRetrieve{id: defaultID + 1}, "IsNotFound", step,
	}, {
		TestQueryByID{limit: 10, out: []snippet{
			snippet{ID: defaultID, Name: defaultName, Code: defaultCode},
		}}, "", step,
	}, {
		TestQueryByID{id: defaultID, limit: 10, out: []snippet{}}, "", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID, Code: "code1",
		}, id: defaultID}, "", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID, Name: "other name",
		}, id: defaultID}, "IsAny", step,
	}, {
		TestRetrieve{id: defaultID, out: snippet{
			ID: defaultID, Name: defaultName, Code: "code1", Modified: base.Add(5 * step),
		}}, "", step,
	}, {
		TestCreate{
			in: snippet{Name: "resonance cascade", Code: "code2"}, id: defaultID + 1,
		}, "", step,
	}, {
		TestCreate{
			in: snippet{Name: "gordon freeman", Code: "code3"}, id: defaultID + 2,
		}, "", step,
	}, {
		TestCreate{
			in: snippet{Name: "live free die hard", Code: "code4"}, id: defaultID + 3,
		}, "", step,
	}, {
		TestCreate{
			in: snippet{Name: "", Code: "no title"}, id: 0,
		}, "IsAny", step,
	}, {
		TestCreate{
			in: snippet{ID: defaultID + 4, Name: "assign id", Code: "code"}, id: 0,
		}, "IsAny", step,
	}, {
		TestQueryByID{id: defaultID, limit: 2, out: []snippet{
			{ID: defaultID + 1, Created: base.Add(8 * step), Modified: base.Add(8 * step), Name: "resonance cascade", Code: "code2"},
			{ID: defaultID + 2, Created: base.Add(9 * step), Modified: base.Add(9 * step), Name: "gordon freeman", Code: "code3"},
		}}, "", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID + 2, Code: "code3a",
		}, id: defaultID + 2}, "", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID + 8, Code: "not found",
		}, id: defaultID + 8}, "IsNotFound", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID + 8, Code: "mismatching id",
		}, id: defaultID + 2}, "IsAny", step,
	}, {
		TestUpdate{in: snippet{
			ID: 0, Code: "invalid zero id",
		}, id: 0}, "IsAny", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID + 2, Code: "modified date", Modified: base,
		}, id: defaultID + 2}, "IsAny", step,
	}, {
		TestQueryByName{name: "resonance", limit: 10, out: []snippet{
			{ID: defaultID + 1, Created: base.Add(8 * step), Modified: base.Add(8 * step), Name: "resonance cascade", Code: "code2"},
		}}, "", step,
	}, {
		TestUpdate{in: snippet{
			ID: defaultID + 1, Name: "cascading failure",
		}, id: defaultID + 1}, "", step,
	}, {
		TestQueryByName{name: "resonance", limit: 10, out: []snippet{}}, "", step,
	}, {
		TestDelete{id: 0}, "IsAny", step,
	}, {
		TestDelete{id: defaultID}, "IsAny", step,
	}, {
		TestDelete{id: defaultID + 1}, "", step,
	}, {
		TestQueryByName{name: "cascad", limit: 10, out: []snippet{}}, "", step,
	}, {
		TestReopen{}, "", step,
	}, {
		TestQueryByName{name: "", limit: 10, out: []snippet{
			{ID: defaultID + 3, Created: base.Add(10 * step), Modified: base.Add(10 * step), Name: "live free die hard", Code: "code4"},
			{ID: defaultID + 0, Modified: base.Add(5 * step), Name: "Default snippet", Code: "code1"},
			{ID: defaultID + 2, Created: base.Add(9 * step), Modified: base.Add(14 * step), Name: "gordon freeman", Code: "code3a"},
		}}, "", step,
	}, {
		TestCreate{in: snippet{Name: "joshua tree", Code: "code5"}, id: defaultID + 4}, "", step,
	}, {
		TestCreate{in: snippet{Name: "duplicate clone", Code: "code6"}, id: defaultID + 5}, "", step,
	}, {
		TestCreate{in: snippet{Name: "duplicate clone", Code: "code7"}, id: defaultID + 6}, "", step,
	}, {
		TestCreate{in: snippet{Name: "duplicate clone", Code: "code8"}, id: defaultID + 7}, "", step,
	}, {
		TestCreate{in: snippet{Name: "burrow", Code: "code9"}, id: defaultID + 8}, "", step,
	}, {
		TestCreate{in: snippet{Name: "transport control protocol", Code: "code10"}, id: defaultID + 9}, "", step,
	}, {
		TestCreate{in: snippet{Name: "user datagram protocol", Code: "code11"}, id: defaultID + 10}, "", step,
	}, {
		TestCreate{in: snippet{Name: "jasmine tea", Code: "code12"}, id: defaultID + 11}, "", step,
	}, {
		TestReopen{}, "", step,
	}, {
		TestCreate{in: snippet{Name: "green tea", Code: "code13"}, id: defaultID + 12}, "", step,
	}, {
		TestCreate{in: snippet{Name: "cherry tea", Code: "code14"}, id: defaultID + 13}, "", step,
	}, {
		TestCreate{in: snippet{Name: "java tea", Code: "code15"}, id: defaultID + 14}, "", step,
	}, {
		TestCreate{in: snippet{Name: "delicious sticky rice", Code: "code16"}, id: defaultID + 15}, "", step,
	}, {
		TestCreate{in: snippet{Name: "super duper ice cream", Code: "code17"}, id: defaultID + 16}, "", step,
	}, {
		TestCreate{in: snippet{Name: "ice cubes in the hot sun", Code: "code18"}, id: defaultID + 17}, "", step,
	}, {
		TestUpdate{in: snippet{ID: defaultID + 5, Code: "code6a"}, id: defaultID + 5}, "", 0,
	}, {
		TestUpdate{in: snippet{ID: defaultID + 6, Code: "code7a"}, id: defaultID + 6}, "", 0,
	}, {
		TestUpdate{in: snippet{ID: defaultID + 7, Code: "code8a"}, id: defaultID + 7}, "", step,
	}, {
		TestUpdate{in: snippet{ID: defaultID + 17, Code: "code18a"}, id: defaultID + 17}, "", step,
	}, {
		TestUpdate{in: snippet{ID: defaultID, Code: "code0a"}, id: defaultID}, "", step,
	}, {
		TestUpdate{in: snippet{ID: defaultID + 9, Code: "code10a"}, id: defaultID + 9}, "", step,
	}, {
		TestUpdate{in: snippet{ID: defaultID + 10, Code: "code11a"}, id: defaultID + 10}, "", step,
	}, {
		TestQueryByName{name: "duplicate ice", limit: 5, out: []snippet{
			{ID: defaultID + 15, Created: base.Add(40 * step), Modified: base.Add(40 * step), Name: "delicious sticky rice", Code: "code16"},
			{ID: defaultID + 7, Created: base.Add(31 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code8a"},
			{ID: defaultID + 6, Created: base.Add(30 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code7a"},
			{ID: defaultID + 5, Created: base.Add(29 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code6a"},
			{ID: defaultID + 17, Created: base.Add(42 * step), Modified: base.Add(44 * step), Name: "ice cubes in the hot sun", Code: "code18a"},
		}}, "", step,
	}, {
		TestQueryByModified{limit: 5, out: []snippet{
			{ID: defaultID + 10, Created: base.Add(34 * step), Modified: base.Add(47 * step), Name: "user datagram protocol", Code: "code11a"},
			{ID: defaultID + 9, Created: base.Add(33 * step), Modified: base.Add(46 * step), Name: "transport control protocol", Code: "code10a"},
			{ID: defaultID + 0, Modified: base.Add(45 * step), Name: "Default snippet", Code: "code0a"},
			{ID: defaultID + 17, Created: base.Add(42 * step), Modified: base.Add(44 * step), Name: "ice cubes in the hot sun", Code: "code18a"},
			{ID: defaultID + 7, Created: base.Add(31 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code8a"},
		}}, "", step,
	}, {
		TestReopen{}, "", step,
	}, {
		TestQueryByModified{modified: base.Add(43 * step), id: defaultID + 7, limit: 10, out: []snippet{
			{ID: defaultID + 6, Created: base.Add(30 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code7a"},
			{ID: defaultID + 5, Created: base.Add(29 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code6a"},
			{ID: defaultID + 16, Created: base.Add(41 * step), Modified: base.Add(41 * step), Name: "super duper ice cream", Code: "code17"},
			{ID: defaultID + 15, Created: base.Add(40 * step), Modified: base.Add(40 * step), Name: "delicious sticky rice", Code: "code16"},
			{ID: defaultID + 14, Created: base.Add(39 * step), Modified: base.Add(39 * step), Name: "java tea", Code: "code15"},
			{ID: defaultID + 13, Created: base.Add(38 * step), Modified: base.Add(38 * step), Name: "cherry tea", Code: "code14"},
			{ID: defaultID + 12, Created: base.Add(37 * step), Modified: base.Add(37 * step), Name: "green tea", Code: "code13"},
			{ID: defaultID + 11, Created: base.Add(35 * step), Modified: base.Add(35 * step), Name: "jasmine tea", Code: "code12"},
			{ID: defaultID + 8, Created: base.Add(32 * step), Modified: base.Add(32 * step), Name: "burrow", Code: "code9"},
			{ID: defaultID + 4, Created: base.Add(28 * step), Modified: base.Add(28 * step), Name: "joshua tree", Code: "code5"},
		}}, "", step,
	}, {
		TestQueryByModified{modified: base.Add(28 * step), id: defaultID + 4, limit: 10, out: []snippet{
			{ID: defaultID + 2, Created: base.Add(9 * step), Modified: base.Add(14 * step), Name: "gordon freeman", Code: "code3a"},
			{ID: defaultID + 3, Created: base.Add(10 * step), Modified: base.Add(10 * step), Name: "live free die hard", Code: "code4"},
		}}, "", step,
	}, {
		TestQueryByModified{modified: base.Add(0 * step), id: defaultID, limit: 10, out: []snippet{}}, "", step,
	}, {
		TestQueryByModified{limit: 0}, "", step,
	}, {
		TestQueryByID{limit: 0}, "", step,
	}, {
		TestQueryByID{limit: -1, out: []snippet{
			{ID: defaultID + 0, Modified: base.Add(45 * step), Name: "Default snippet", Code: "code0a"},
			{ID: defaultID + 2, Created: base.Add(9 * step), Modified: base.Add(14 * step), Name: "gordon freeman", Code: "code3a"},
			{ID: defaultID + 3, Created: base.Add(10 * step), Modified: base.Add(10 * step), Name: "live free die hard", Code: "code4"},
			{ID: defaultID + 4, Created: base.Add(28 * step), Modified: base.Add(28 * step), Name: "joshua tree", Code: "code5"},
			{ID: defaultID + 5, Created: base.Add(29 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code6a"},
			{ID: defaultID + 6, Created: base.Add(30 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code7a"},
			{ID: defaultID + 7, Created: base.Add(31 * step), Modified: base.Add(43 * step), Name: "duplicate clone", Code: "code8a"},
			{ID: defaultID + 8, Created: base.Add(32 * step), Modified: base.Add(32 * step), Name: "burrow", Code: "code9"},
			{ID: defaultID + 9, Created: base.Add(33 * step), Modified: base.Add(46 * step), Name: "transport control protocol", Code: "code10a"},
			{ID: defaultID + 10, Created: base.Add(34 * step), Modified: base.Add(47 * step), Name: "user datagram protocol", Code: "code11a"},
			{ID: defaultID + 11, Created: base.Add(35 * step), Modified: base.Add(35 * step), Name: "jasmine tea", Code: "code12"},
			{ID: defaultID + 12, Created: base.Add(37 * step), Modified: base.Add(37 * step), Name: "green tea", Code: "code13"},
			{ID: defaultID + 13, Created: base.Add(38 * step), Modified: base.Add(38 * step), Name: "cherry tea", Code: "code14"},
			{ID: defaultID + 14, Created: base.Add(39 * step), Modified: base.Add(39 * step), Name: "java tea", Code: "code15"},
			{ID: defaultID + 15, Created: base.Add(40 * step), Modified: base.Add(40 * step), Name: "delicious sticky rice", Code: "code16"},
			{ID: defaultID + 16, Created: base.Add(41 * step), Modified: base.Add(41 * step), Name: "super duper ice cream", Code: "code17"},
			{ID: defaultID + 17, Created: base.Add(42 * step), Modified: base.Add(44 * step), Name: "ice cubes in the hot sun", Code: "code18a"},
		}}, "", step,
	}, {
		TestCreate{in: snippet{Name: "\n"}}, "IsRequestError", step,
	}, {
		TestUpdate{in: snippet{Name: "\n"}, id: defaultID + 5}, "IsRequestError", step,
	}}

	for i, tt := range tests {
		var err error
		switch tc := tt.test.(type) {
		case TestCreate:
			var id int64
			id, err = db.Create(tc.in)
			if err == nil && id != tc.id {
				t.Fatalf("test %d, Create(%v) = %d, want %d", i, tc.in, id, tc.id)
			}
		case TestRetrieve:
			var out snippet
			out, err = db.Retrieve(tc.id)
			if err == nil && !equalSnippet(out, tc.out) {
				t.Fatalf("test %d, Retrieve(%d):\ngot  %v\nwant %v", i, tc.id, out, tc.out)
			}
		case TestUpdate:
			err = db.Update(tc.in, tc.id)
		case TestDelete:
			err = db.Delete(tc.id)
		case TestQueryByModified:
			var out []snippet
			out, err = db.QueryByModified(tc.modified, tc.id, tc.limit)
			if err == nil && !equalSnippets(out, tc.out) {
				t.Fatalf("test %d, QueryByModified(%v, %d):\ngot  %v\nwant %v", i, tc.modified, tc.id, out, tc.out)
			}
		case TestQueryByID:
			var out []snippet
			out, err = db.QueryByID(tc.id, tc.limit)
			if err == nil && !equalSnippets(out, tc.out) {
				t.Fatalf("test %d, QueryByID(%d):\ngot  %v\nwant %v", i, tc.id, out, tc.out)
			}
		case TestQueryByName:
			var out []snippet
			out, err = db.QueryByName(tc.name, tc.limit)
			if err == nil && !equalSnippets(out, tc.out) {
				t.Fatalf("test %d, QueryByName(%v):\ngot  %v\nwant %v", i, tc.name, out, tc.out)
			}
		case TestReopen:
			err = db.Close()
			closer = func() error { return nil }
			if err != nil {
				t.Fatalf("test %d, Close error: %v", i, err)
			}
			db, err = openDatabase(tmpDir)
			if err != nil {
				t.Fatalf("test %d, openDatabase error: %v", i, err)
			}
			closer = db.Close
			db.timeNow = func() time.Time { return now }
		default:
			t.Fatalf("test %d, unknown test type: %T", i, tt.test)
		}
		if tt.errf != "" && !errFuncs[tt.errf](err) {
			t.Fatalf("test %d, mismatching error:\ngot %v\nwant %s(err) == true", i, err, tt.errf)
		} else if tt.errf == "" && err != nil {
			t.Fatalf("test %d, unexpected error: got %v", i, err)
		}
		now = now.Add(tt.add)
	}
}
