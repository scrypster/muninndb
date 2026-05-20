package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
)

// captureStdout redirects os.Stdout during f() and returns the captured output.
// The pipe is drained concurrently to avoid deadlock when f() produces more
// output than the OS pipe buffer (≈4KB on Windows, 64KB on Linux).
func captureStdout(f func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic("captureStdout: os.Pipe: " + err.Error())
	}
	old := os.Stdout
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		r.Close()
		close(done)
	}()

	f()

	w.Close()
	os.Stdout = old
	<-done
	return buf.String()
}

// captureStderr redirects os.Stderr during f() and returns the captured output.
// The pipe is drained concurrently to avoid deadlock when f() produces more
// output than the OS pipe buffer (≈4KB on Windows, 64KB on Linux).
func captureStderr(f func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic("captureStderr: os.Pipe: " + err.Error())
	}
	old := os.Stderr
	os.Stderr = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		r.Close()
		close(done)
	}()

	f()

	w.Close()
	os.Stderr = old
	<-done
	return buf.String()
}

// newHealthServer starts a test HTTP server that returns 200 on any request.
// Caller must call Close() when done.
func newHealthServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// newTLSHealthServer starts a TLS-only test server that returns 200 on any
// request. Caller must call Close() when done.
func newTLSHealthServer() *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// newJSONServer starts a test HTTP server that returns the given JSON body with status.
func newJSONServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

// newAuthServer starts a server that returns 200 with a Set-Cookie header.
func newAuthServer(cookieName, cookieValue string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: cookieValue})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}
