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

// streamgate passes data through; it does not manufacture any. An earlier
// version filled encoder silences with null packets, and those packets went
// through readPacket exactly like real ones -- so synthetic data drove
// streamgate's own state machine and the ALIGN_TIMEOUT fallback fired during a
// silence instead of waiting for the encoder. A silent encoder must produce a
// failure, never invented bytes.
func TestIdleReaderNeverSynthesises(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	r := newIdleReader(&config{tuner: "T"}, pr, 200*time.Millisecond)

	b := make([]byte, 4096)
	n, err := r.Read(b)
	if err == nil {
		t.Fatalf("a silent encoder produced %d bytes; nothing may be invented here", n)
	}
	if !strings.Contains(err.Error(), "sent nothing") {
		t.Errorf("failed with %v, want the silence to be named", err)
	}
}

// A stream that is flowing must pass through byte-exact, with nothing added,
// dropped or reordered.
func TestIdleReaderPassesDataThrough(t *testing.T) {
	pr, pw := io.Pipe()
	r := newIdleReader(&config{tuner: "T"}, pr, 5*time.Second)

	want := buildStream(4, 10)
	go func() {
		_, _ = pw.Write(want)
		_ = pw.Close()
	}()

	got, err := io.ReadAll(r)
	if err != nil && err != io.EOF {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("passed through %d bytes, want the %d written unmodified", len(got), len(want))
	}
}

// Nothing may be held back. Whatever has arrived must be available at once, or
// this layer has become the buffer it must never be.
func TestIdleReaderHoldsNothingBack(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	r := newIdleReader(&config{tuner: "T"}, pr, 5*time.Second)

	pkt := buildStream(1, 10)
	go func() { _, _ = pw.Write(pkt) }()

	b := make([]byte, 4096)
	start := time.Now()
	n, err := r.Read(b)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if n != len(pkt) {
		t.Errorf("read %d of %d bytes that had already arrived", n, len(pkt))
	}
	if waited := time.Since(start); waited > 50*time.Millisecond {
		t.Errorf("waited %v for data that was already there", waited)
	}
}

// READ_TIMEOUT is measured over the silence, not over the connection: a stream
// that keeps arriving must never trip it, however long the recording runs.
func TestIdleReaderBudgetResetsOnData(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	r := newIdleReader(&config{tuner: "T"}, pr, 120*time.Millisecond)

	pkt := buildStream(1, 10)
	go func() {
		for {
			time.Sleep(30 * time.Millisecond)
			if _, err := pw.Write(pkt); err != nil {
				return
			}
		}
	}()

	b := make([]byte, tsPacketSize)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := r.Read(b); err != nil {
			t.Fatalf("budget tripped on a stream that kept arriving: %v", err)
		}
	}
}
