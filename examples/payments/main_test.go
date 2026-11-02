package main

import (
	"net/http/httptest"
	"testing"

	"connectrpc.com/grpchealth"

	"github.com/trendvidia/voya/examples/internal/demo"
)

// TestServesHealth holds the quick start's `voya health` line: a service that
// stops serving grpc.health.v1 shows `error` there while it is healthy. The
// empty name is the whole server, which is what `voya health` asks about.
func TestServesHealth(t *testing.T) {
	mux, err := newMux("test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := grpchealth.NewClient(srv.Client(), srv.URL)

	for _, service := range []string{"", demo.Service} {
		res, err := client.Check(t.Context(), &grpchealth.CheckRequest{Service: service})
		if err != nil {
			t.Fatalf("health check of %q: %v", service, err)
		}
		if res.Status != grpchealth.StatusServing {
			t.Fatalf("status of %q = %s, want serving", service, res.Status)
		}
	}
}
