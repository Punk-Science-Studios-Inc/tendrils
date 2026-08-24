package buildinfo

import (
	"encoding/json"
	"os"
	"testing"
)

// The wire format a build speaks and the wire format its release announces are
// two copies of one fact: the constant here, and release.json which GoReleaser
// publishes as a release asset for `tendrils upgrade` to read. If they drift, a
// release that changes the wire format ships without the marker that makes
// `upgrade` stop and ask — which is exactly the failure the marker exists to
// prevent, and it would be silent.
func TestReleaseJSONDeclaresThisWireFormat(t *testing.T) {
	data, err := os.ReadFile("../../release.json")
	if err != nil {
		t.Fatalf("release.json is published as a release asset and must exist: %v", err)
	}
	var meta struct {
		WireFormat    int    `json:"wire_format"`
		MinCompatible string `json:"min_compatible"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("release.json must be valid JSON — the updater treats an unreadable one as a wire-format boundary: %v", err)
	}
	if meta.WireFormat != WireFormat {
		t.Errorf("release.json declares wire_format %d, this build speaks %d; bump both together", meta.WireFormat, WireFormat)
	}
	if meta.MinCompatible == "" {
		t.Error("release.json must name a min_compatible version")
	}
}
