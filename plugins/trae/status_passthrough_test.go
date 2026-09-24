package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestErrorEnvelopeForCarriesStatus(t *testing.T) {
	raw := errorEnvelopeFor(&statusError{status: http.StatusPaymentRequired, err: errors.New("upstream 402")})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got ok=%v", env.OK)
	}
	if env.Error.HTTPStatus != http.StatusPaymentRequired {
		t.Fatalf("http_status = %d, want 402", env.Error.HTTPStatus)
	}
}

func TestErrorEnvelopeForPlainErrorOmitsStatus(t *testing.T) {
	raw := errorEnvelopeFor(errors.New("plain failure"))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error == nil || env.Error.HTTPStatus != 0 {
		t.Fatalf("expected http_status omitted, got %+v", env.Error)
	}
}

// TestUpstreamStatusErrorPolicy preserves request and server failures alongside
// account failures. A plain HTTP 404 stays status-less —
// the host maps 404 to 12h while the plugin pool intends CoolSoft 60s.
func TestUpstreamStatusErrorPolicy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"402 payment", 402, 402},
		{"401 dead token", 401, 401},
		{"429 rate limit", 429, 429},
		{"404 not found", 404, 0},
		{"403 plan limit body", 403, 0},
		{"413 input too large", 413, 413},
		{"400 client", 400, 400},
		{"422 model mismatch", 422, 422},
		{"500 server", 500, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := errors.New("translated")
			err := upstreamStatusError(tc.status, base)
			var se *statusError
			if tc.want == 0 {
				if errors.As(err, &se) {
					t.Fatalf("status %d: expected plain error, got statusError{%d}", tc.status, se.status)
				}
				if !errors.Is(err, base) {
					t.Fatalf("plain error must wrap base")
				}
				return
			}
			if !errors.As(err, &se) {
				t.Fatalf("status %d: expected statusError", tc.status)
			}
			if se.status != tc.want {
				t.Fatalf("status = %d, want %d", se.status, tc.want)
			}
		})
	}
}
