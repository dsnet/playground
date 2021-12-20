// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dsnet/golib/jsonfmt"
	"golang.org/x/crypto/ssh/terminal"
)

// Version of the playground binary. May be set by linker when building.
var version string

const Help = `
Playground is a server application for running Go snippets over the browser.

The JSON configuration file takes the following form:
{
	// The socket address to serve on (default is localhost:8080).
	"ServeAddress": "",

	// Path to a file to output the log (default is stdout).
	"LogFile": "",

	// If PasswordHash is set, then the server will require the user to login
	// using some pre-determined password. This configuration file does not
	// store the password itself, but a SHA256 hashed version of the password.
	// The following script can be used to generate a salt and hash pair.
	//
	//  #!/bin/bash
	//  read -s -p "Password: " PASSWORD && echo
	//  PASSWORD_SALT=$(head -c 1024 /dev/urandom | sha256sum | head -c 64)
	//  PASSWORD_HASH=$(echo -n "$(echo $PASSWORD_SALT | xxd -r -p)${PASSWORD}" | sha256sum | head -c 64)
	//  echo -en "PasswordSalt: $PASSWORD_SALT\nPasswordHash: $PASSWORD_HASH\n"
	//  unset PASSWORD PASSWORD_SALT PASSWORD_HASH
	//
	// The password fields must be set.
	"PasswordSalt": "",
	"PasswordHash": "",

	// Specifying a TLS certificate and key file will enable the server to serve
	// over HTTPS instead of HTTP.
	//
	// The TLS fields must be set if ServeAddress does not listen on the
	// loopback network interface.
	"TLSCertFile": "",
	"TLSKeyFile": "",

	// Path to the directory where persistent server data is to be stored.
	// This can be a full path or a relative path to the CWD.
	//
	// If not set, this defaults to "$HOME/.playground"
	"DataPath": "",

	// Path to the default binary used to build Go code.
	// This can be a file path or a single binary name (located in the $PATH).
	//
	// Defaults to "go".
	"GoBinary": "",

	// Path to the binary used to format Go source code.
	// This can be a file path or a single binary name (located in the $PATH).
	//
	// Defaults to "goimports" if available, otherwise "gofmt".
	"FmtBinary": "",

	// GoVersions is a map of various versions of Go available on the system.
	// It is useful to have multiple versions so that benchmarks can be tested
	// on a variety of Go versions.
	//
	// The key is an identifier for a given Go version (e.g., go1.3).
	// The value is a file path or a single binary name (located in the $PATH).
	//
	// It is valid for the map to be empty.
	"GoVersions": {},

	// Environment is a map of environment variables to set.
	"Environment": {},
}`

type config struct {
	ServeAddress string            `json:",omitempty"`
	LogFile      string            `json:",omitempty"`
	PasswordSalt string            `json:",omitempty"`
	PasswordHash string            `json:",omitempty"`
	TLSCertFile  string            `json:",omitempty"`
	TLSKeyFile   string            `json:",omitempty"`
	DataPath     string            `json:",omitempty"`
	GoBinary     string            `json:",omitempty"`
	FmtBinary    string            `json:",omitempty"`
	GoVersions   map[string]string `json:",omitempty"`
	Environment  map[string]string `json:",omitempty"`
}

func loadConfig(path string) (conf config, logger *log.Logger, closer func() error) {
	var logBuf bytes.Buffer
	logger = log.New(io.MultiWriter(os.Stderr, &logBuf), "", log.Ldate|log.Ltime|log.Lshortfile)

	var hash string
	if b, _ := ioutil.ReadFile(os.Args[0]); len(b) > 0 {
		hash = fmt.Sprintf("%x", sha256.Sum256(b))
	}

	// Load configuration file.
	if path != "" {
		c, err := ioutil.ReadFile(path)
		if err != nil {
			logger.Fatalf("unable to read config: %v", err)
		}
		if c, err = jsonfmt.Format(c, jsonfmt.Standardize()); err != nil {
			logger.Fatalf("unable to parse config: %v", err)
		}
		if err := json.Unmarshal(c, &conf); err != nil {
			logger.Fatalf("unable to decode config: %v", err)
		}
	} else {
		fmt.Print("Enter a new Playground login password: ")
		p, err := terminal.ReadPassword(int(syscall.Stdin))
		fmt.Println()
		if err != nil {
			logger.Fatalf("unable to read password: %v", err)
		}
		if len(bytes.TrimSpace(p)) < 8 {
			logger.Fatal("error: insecure password")
		}
		s, err := ioutil.ReadAll(&io.LimitedReader{R: rand.Reader, N: 32})
		if err != nil {
			logger.Fatalf("unable to generate salt: %v", err)
		}
		conf.PasswordSalt = fmt.Sprintf("%x", s)
		conf.PasswordHash = fmt.Sprintf("%x", sha256.Sum256(append(s, p...)))
	}

	// Set default values.
	if conf.ServeAddress == "" {
		conf.ServeAddress = "localhost:8080"
	}
	if conf.DataPath == "" {
		conf.DataPath = filepath.Join(os.Getenv("HOME"), ".playground")
	}
	if conf.GoBinary == "" {
		conf.GoBinary = "go"
	}
	if conf.FmtBinary == "" {
		// Use goimports if available, otherwise fall back to gofmt.
		conf.FmtBinary = "goimports"
		cmd := exec.Command(conf.FmtBinary, "-h")
		if err := cmd.Start(); err != nil {
			conf.FmtBinary = "gofmt"
		} else {
			cmd.Process.Kill()
		}
	}

	// Print the configuration.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "\t")
	enc.Encode(struct {
		config
		BinaryVersion string `json:",omitempty"`
		BinarySHA256  string `json:",omitempty"`
	}{conf, version, hash})
	logger.Printf("loaded config:\n%s", b.String())

	// Setup the log output.
	if conf.LogFile == "" {
		logger.SetOutput(os.Stderr)
		closer = func() error { return nil }
	} else {
		f, err := os.OpenFile(conf.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0664)
		if err != nil {
			logger.Fatalf("error opening log file: %v", err)
		}
		f.Write(logBuf.Bytes()) // Write log output prior to this point
		logger.Printf("suppress stderr logging (redirected to %s)", f.Name())
		logger.SetOutput(f)
		closer = f.Close
	}

	// Check security settings.
	hasPass := conf.PasswordSalt != "" || conf.PasswordHash != ""
	reHex := regexp.MustCompile(`^[0-9a-fA-F]{64}$`) // SHA256 hash in hex
	if hasPass && !(reHex.MatchString(conf.PasswordSalt) && reHex.MatchString(conf.PasswordHash)) {
		logger.Fatal("PasswordSalt and PasswordHash must be 32 byte long hex-strings")
	}

	// Apply environment variables.
	for k, v := range conf.Environment {
		os.Setenv(k, v)
	}

	// Create the data directory if necessary.
	if _, err := os.Stat(conf.DataPath); os.IsNotExist(err) {
		if err := os.Mkdir(conf.DataPath, 0775); err != nil {
			logger.Fatalf("unable to create directory: %v", err)
		}
	}

	return conf, logger, closer
}

func main() {
	if len(os.Args) > 2 || (len(os.Args) == 2 && strings.HasPrefix(os.Args[1], "-")) {
		fmt.Fprintf(os.Stderr, "Usage: %s [CONF_FILE]\n%s\n", os.Args[0], Help)
		os.Exit(1)
	}

	// Parse the configuration file.
	var confPath string
	if len(os.Args) == 2 {
		confPath = os.Args[1]
	}
	conf, logger, closer := loadConfig(confPath)
	defer closer()

	// Register shutdown hook.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		logger.Printf("received %v - initiating shutdown", <-sigc)
		cancel()
	}()

	logger.Printf("%s starting on %v", path.Base(os.Args[0]), conf.ServeAddress)
	defer logger.Printf("%s shutdown", path.Base(os.Args[0]))

	// Start the server.
	var pwHash, pwSalt [sha256.Size]byte
	if conf.PasswordHash != "" || conf.PasswordSalt != "" {
		hex.Decode(pwHash[:], []byte(conf.PasswordHash))
		hex.Decode(pwSalt[:], []byte(conf.PasswordSalt))
	}
	pg, err := newPlayground(pwHash, pwSalt, conf.DataPath, conf.GoBinary, conf.FmtBinary, conf.GoVersions, logger)
	if err != nil {
		logger.Fatalf("newPlayground error: %v", err)
	}
	defer pg.Close()

	server := &http.Server{
		Addr:     conf.ServeAddress,
		Handler:  pg,
		ErrorLog: log.New(ioutil.Discard, "", 0),
	}
	defer server.Close()
	go func() {
		for {
			var err error
			if conf.TLSCertFile != "" || conf.TLSKeyFile != "" {
				err = server.ListenAndServeTLS(conf.TLSCertFile, conf.TLSKeyFile)
			} else {
				err = server.ListenAndServe()
			}
			if err != nil {
				select {
				case <-ctx.Done(): // Ignore error when closing
				default:
					logger.Printf("ListenAndServe error: %v", err)
				}
			}
			time.Sleep(30 * time.Second)
		}
	}()
	<-ctx.Done()
}
