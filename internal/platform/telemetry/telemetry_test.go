package telemetry

import "testing"

func TestSignalEndpoint(t *testing.T) {
	t.Parallel()

	got := signalEndpoint("http://localhost:4318/", "/v1/traces")
	if got != "http://localhost:4318/v1/traces" {
		t.Fatalf("signalEndpoint() = %q", got)
	}
}
