package resources

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestStartupConsentBoundaryAndInvocationOnly(t *testing.T) {
	for _, c := range []struct {
		name                          string
		limit, total                  uint64
		allow, checkOnly, interactive bool
		answer                        string
		restricted, fail              bool
	}{
		{name: "below unattended", limit: 39, total: 100, fail: true},
		{name: "exact boundary", limit: 40, total: 100},
		{name: "above boundary", limit: 41, total: 100},
		{name: "explicit invocation", limit: 39, total: 100, allow: true, restricted: true},
		{name: "check does not serve", limit: 39, total: 100, checkOnly: true, restricted: true},
		{name: "interactive accept", limit: 39, total: 100, interactive: true, answer: "yes\n", restricted: true},
		{name: "interactive refuse", limit: 39, total: 100, interactive: true, answer: "no\n", fail: true},
		{name: "interactive eof", limit: 39, total: 100, interactive: true, answer: "yes", fail: true},
		{name: "pipe cannot consent", limit: 39, total: 100, answer: "yes\n", fail: true},
		{name: "large exact boundary", limit: math.MaxUint64 / 5 * 2, total: math.MaxUint64},
		{name: "large below boundary", limit: math.MaxUint64/5*2 - 1, total: math.MaxUint64, allow: true, restricted: true},
		{name: "zero total", limit: 1, total: 0, fail: true},
		{name: "zero allowance", limit: 0, total: 100, fail: true},
		{name: "invalid allowance", limit: 101, total: 100, fail: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var output bytes.Buffer
			restricted, err := startupConsent(c.limit, c.total, c.allow, c.checkOnly, c.interactive, strings.NewReader(c.answer), &output)
			if restricted != c.restricted || (err != nil) != c.fail {
				t.Fatalf("restricted=%t err=%v", restricted, err)
			}
			if c.limit == 39 && (!strings.Contains(output.String(), "39 bytes of 100 system bytes (39.00%)") || !strings.Contains(output.String(), "concurrency to 1") || !strings.Contains(output.String(), "disables AVIF")) {
				t.Fatal("Missing startup consequences", output.String())
			}
		})
	}
	// Accepting one invocation cannot authorize the next.
	var output bytes.Buffer
	if _, err := startupConsent(1, 100, false, false, false, strings.NewReader("yes\n"), &output); err == nil {
		t.Fatal("Consent persisted")
	}
}
