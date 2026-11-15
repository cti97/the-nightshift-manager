package provider_test

import (
	"encoding/json"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// TestConformanceIsCallableFromAnotherPackage is the compile-time proof of the
// suite's contract: an adapter's test in package x_test can call
// provider.Conformance with nothing but the exported Provider interface and a
// *testing.T. It runs the same six checks as the in-package stub test, so a
// failure here is a failure of the exported surface rather than of the fixture.
func TestConformanceIsCallableFromAnotherPackage(t *testing.T) {
	provider.Conformance(t, func() provider.Provider {
		return provider.NewStub([]provider.Response{
			{
				Content: "The brief asks for the provider seam.",
				ToolCalls: []provider.Call{{
					ID:        "call-1",
					Name:      "read_file",
					Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`),
				}},
				Usage: provider.Usage{In: 120, Out: 30},
				Stop:  "tool_use",
			},
			{
				Content: "The file was read and the brief is planned.",
				Usage:   provider.Usage{In: 120, Out: 30},
				Stop:    "end_turn",
			},
		})
	})
}
