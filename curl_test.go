package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A typo must not silently change who delivers recordings.
func TestDeliveryMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "internal"},
		{"curl", "curl"},
		{"CURL", "curl"},
		{`"curl"`, "curl"}, // docker --env-file keeps the quotes
		{" internal ", "internal"},
		{"crul", "internal"},
	}
	for _, c := range cases {
		os.Setenv("DELIVERY", c.in)
		if got := deliveryMode(); got != c.want {
			t.Errorf("deliveryMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	os.Unsetenv("DELIVERY")
}

// The embedded bytes must be a runnable curl of the version the log claims,
// not merely bytes that were once copied in. Skipped where no curl is bundled
// (non-linux dev builds); the manual CI dispatch runs it on linux, which makes
// it part of the release gate.
func TestBundledCurlRuns(t *testing.T) {
	if len(curlBin) == 0 {
		t.Skip("no curl bundled for this platform")
	}
	if runtime.GOOS != "linux" {
		t.Skip("bundled curl is a linux binary")
	}
	bin := filepath.Join(t.TempDir(), "curl")
	if err := os.WriteFile(bin, curlBin, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("bundled curl does not run: %v", err)
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	if !strings.HasPrefix(first, "curl "+curlVersion) {
		t.Errorf("bundled curl reports %q, want version %s -- update curlVersion or third_party", first, curlVersion)
	}
}
