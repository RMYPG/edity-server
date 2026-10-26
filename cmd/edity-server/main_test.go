package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
)

func TestTheServerBoundsEveryPhaseOfAConnection(t *testing.T) {
	srv := httpServer(":0", http.NotFoundHandler(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A zero timeout means none: a client that sends its headers and then
	// withholds the body, or keeps a connection idle, would hold it forever.
	for name, timeout := range map[string]int64{
		"ReadHeaderTimeout": int64(srv.ReadHeaderTimeout),
		"ReadTimeout":       int64(srv.ReadTimeout),
		"WriteTimeout":      int64(srv.WriteTimeout),
		"IdleTimeout":       int64(srv.IdleTimeout),
	} {
		if timeout <= 0 {
			t.Errorf("%s is not set", name)
		}
	}
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Errorf("ReadTimeout %v is shorter than ReadHeaderTimeout %v", srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
}
