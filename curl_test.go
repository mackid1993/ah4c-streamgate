package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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

// An encoder that goes quiet must not take the bitstream down with it: the gap
// is filled with null packets so the DVR keeps receiving. This is the whole
// reason streamgate stays in the byte path.
func TestStallReaderFillsGapsWithNulls(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	sr := newStallReader(&config{tuner: "T"}, pr, 20*time.Millisecond, 5*time.Second)

	b := make([]byte, tsPacketSize)
	start := time.Now()
	n, err := sr.Read(b)
	if err != nil {
		t.Fatalf("read during a stall: %v", err)
	}
	if n != tsPacketSize {
		t.Fatalf("filled %d bytes, want one %d-byte packet", n, tsPacketSize)
	}
	if b[0] != 0x47 {
		t.Errorf("fill does not start on a sync byte: %#x", b[0])
	}
	if got := pid(b); got != 0x1fff {
		t.Errorf("fill has pid %#x, want the null pid 0x1fff", got)
	}
	// It must wait for the gap before filling, not fill eagerly -- otherwise a
	// healthy stream gets nulls interleaved into it for no reason.
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("filled after %v, before the %v gap had elapsed", elapsed, 20*time.Millisecond)
	}
}

// A stream that is flowing must pass through untouched. If the fill ever raced
// real data, every recording would carry stuffing it did not need.
func TestStallReaderPassesDataThrough(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newStallReader(&config{tuner: "T"}, pr, 50*time.Millisecond, 5*time.Second)

	want := buildStream(4, 10)
	go func() {
		_, _ = pw.Write(want)
		_ = pw.Close()
	}()

	got, err := io.ReadAll(sr)
	if err != nil && err != io.EOF {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("passed through %d bytes, want the %d written unmodified", len(got), len(want))
	}
}

// Filling must be bounded. An encoder that never comes back would otherwise
// produce a recording of nulls that never ends, and the tuner slot would be
// held until the DVR gave up.
func TestStallReaderFailsOnceQuietOutrunsTheBudget(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	sr := newStallReader(&config{tuner: "T"}, pr, 10*time.Millisecond, 60*time.Millisecond)

	b := make([]byte, tsPacketSize)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the reader filled past its budget instead of failing")
		}
		if _, err := sr.Read(b); err != nil {
			if !strings.Contains(err.Error(), "sent nothing") {
				t.Errorf("failed with %v, want the silence to be named", err)
			}
			return
		}
	}
}

// The budget is measured over the silence, not over the connection: a stream
// that keeps arriving must never trip it, however long the recording runs.
func TestStallReaderBudgetResetsOnData(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newStallReader(&config{tuner: "T"}, pr, 10*time.Millisecond, 50*time.Millisecond)
	defer pr.Close()

	pkt := buildStream(1, 10)
	// Quiet for longer than the gap, but never for longer than the budget. The
	// writer stops when the test closes the read end under it.
	go func() {
		for {
			time.Sleep(25 * time.Millisecond)
			if _, err := pw.Write(pkt); err != nil {
				return
			}
		}
	}()

	b := make([]byte, tsPacketSize)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := sr.Read(b); err != nil {
			t.Fatalf("budget tripped on a stream that kept arriving: %v", err)
		}
	}
}
