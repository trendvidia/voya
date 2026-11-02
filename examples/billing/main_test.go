package main

import (
	"net/http/httptest"
	"testing"

	"connectrpc.com/grpchealth"
)

// TestServesHealth holds the quick start's `voya health` line: a service that
// stops serving grpc.health.v1 shows `error` there while it is healthy.
func TestServesHealth(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	res, err := grpchealth.NewClient(srv.Client(), srv.URL).Check(t.Context(), &grpchealth.CheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if res.Status != grpchealth.StatusServing {
		t.Fatalf("status = %s, want serving", res.Status)
	}
}
