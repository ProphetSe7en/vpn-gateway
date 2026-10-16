package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A qBittorrent that is not up yet must be reported as not answering,
// not as a failure to read the port: the first check often lands while
// qBit is still starting.
func TestPortCheckClassifiesErrors(t *testing.T) {
	// Closed port: the real case from a container that starts qBit late.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	mapping := PortMapping{Port: port, Name: "qbit", Type: "qbittorrent"}
	_, err = (&qBitPoller{}).ListeningPort(testCtx(t), mapping)
	if err == nil {
		t.Fatal("expected an error against a closed port")
	}
	if !isConnectionError(err) {
		t.Fatalf("isConnectionError(%v) = false, want true", err)
	}

	// An answer we could not use is not a connection error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, hostPort, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(hostPort)
	_, err = (&qBitPoller{}).ListeningPort(testCtx(t), PortMapping{Port: p, Type: "qbittorrent"})
	if err == nil || isConnectionError(err) {
		t.Fatalf("err = %v, isConnectionError = %v, want a non-connection error", err, isConnectionError(err))
	}

	if isConnectionError(errors.New("plain")) || isConnectionError(fmt.Errorf("wrapped: %w", errQBitLoginRequired)) {
		t.Fatal("unrelated errors must not count as connection errors")
	}
}
