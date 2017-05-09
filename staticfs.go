// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

// Code generated by staticfs_gen.go with go1.8. DO NOT EDIT.

package main

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/gob"
	"strings"
)

// staticFS is a mapping from file paths without the leading slash
// to the contents of the file (e.g. css/playground.css => data).
var staticFS = func() (m map[string][]byte) {
	r := strings.NewReader("H4sIAAAAAAAC/+L738jCyPS/iYGRh5GLgYHlfxMDAyAAAP//+kgx6BQAAAA=")
	rx := base64.NewDecoder(base64.StdEncoding, r)
	rz, _ := gzip.NewReader(rx)
	gd := gob.NewDecoder(rz)
	if err := gd.Decode(&m); err != nil {
		panic(err)
	}
	return
}()

// mimeTypes is a mapping from file extensions to MIME types.
var mimeTypes = map[string]string{"css": "text/css; charset=utf-8", "html": "text/html; charset=utf-8", "ico": "image/x-icon", "js": "application/javascript", "svg": "image/svg+xml", "woff": "font/woff"}

// mimeFromPath returns the MIME type based on the file extension in the path.
func mimeFromPath(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return mimeTypes[p[i+1:]]
	}
	return ""
}
