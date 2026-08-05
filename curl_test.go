package main

import (
	"bytes"
	"errors"
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

// burstReader hands over `burst` at memory speed, then serves `tail` one
// packet at a time with a real wait before each -- the arrival shape of an
// encoder's buffered cache followed by live delivery.
type burstReader struct {
	burst, tail []byte
	gapDone     bool
}

func (r *burstReader) Read(p []byte) (int, error) {
	if len(r.burst) > 0 {
		n := copy(p, r.burst)
		r.burst = r.burst[n:]
		return n, nil
	}
	if len(r.tail) == 0 {
		return 0, io.EOF
	}
	if !r.gapDone {
		r.gapDone = true
		time.Sleep(15 * time.Millisecond)
	} else {
		time.Sleep(3 * time.Millisecond)
	}
	n := tsPacketSize
	if n > len(r.tail) {
		n = len(r.tail)
	}
	n = copy(p, r.tail[:n])
	r.tail = r.tail[n:]
	return n, nil
}

func deliverConfig() *config {
	return &config{tuner: "T", alignTimeout: 2 * time.Second, readTimeout: 2 * time.Second}
}

// The promise the head makes: nothing from the encoder's connect burst (the
// channel-change cache) and nothing inside the render margin reaches the
// output, however many keyframes they carry -- and the start is the first
// keyframe past the margin, tables in front, unbroken to the end.
func TestDeliverStartsCleanPastTheMargin(t *testing.T) {
	backlog := buildStream(120, 10) // pre-gate cache, keyframes and all
	var tail []byte
	for i := 0; i < 520; i++ {
		// Keyframes at ~0.6s (inside the 1s margin: must be discarded) and at
		// ~1.35s (the first past it: must be the start). Wide apart so timing
		// jitter cannot flip which side of the margin they land on.
		key := i == 200 || i == 450
		tail = append(tail, videoPacket(byte(i&0x0f), key, 1000+i)...)
	}
	full := append(append([]byte{}, backlog...), tail...)

	var buf bytes.Buffer
	saved := stdoutW
	stdoutW = &buf
	defer func() { stdoutW = saved }()

	wrote, err := deliver(deliverConfig(), &burstReader{burst: backlog, tail: tail}, true, time.Now(), func() {})
	if !wrote || !errors.Is(err, io.EOF) {
		t.Fatalf("deliver = %v, %v; want wrote with the stream ending", wrote, err)
	}
	out := buf.Bytes()
	if len(out) < 3*tsPacketSize {
		t.Fatalf("output is %d bytes, want tables plus video", len(out))
	}
	if pid(out) != 0 || pid(out[tsPacketSize:]) != testPMTPid {
		t.Fatalf("head pids %d,%d; want the cached PAT then PMT", pid(out), pid(out[tsPacketSize:]))
	}
	rest := out[2*tsPacketSize:]
	idx := bytes.Index(full, rest)
	if idx < 0 || idx%tsPacketSize != 0 || idx+len(rest) != len(full) {
		t.Fatal("output is not a contiguous run of the input to its end")
	}
	if !randomAccess(rest[:tsPacketSize]) {
		t.Error("output does not start on a keyframe")
	}
	// Past the whole cache AND past the in-margin keyframe at ~0.6s.
	if idx < len(backlog)+300*tsPacketSize {
		t.Errorf("output starts %d packets into the input -- pre-margin footage leaked through",
			idx/tsPacketSize)
	}
}

// An ungated tune (no TUNERn_IP, or ON_TIMEOUT=stream) is a plain copy from
// the first byte, exactly as those modes are documented.
func TestDeliverUngatedIsAPlainCopy(t *testing.T) {
	input := buildStream(60, 10)

	var buf bytes.Buffer
	saved := stdoutW
	stdoutW = &buf
	defer func() { stdoutW = saved }()

	wrote, err := deliver(deliverConfig(), bytes.NewReader(input), false, time.Now(), func() {})
	if !wrote || !errors.Is(err, io.EOF) {
		t.Fatalf("deliver = %v, %v; want wrote with the stream ending", wrote, err)
	}
	if !bytes.Equal(buf.Bytes(), input) {
		t.Error("ungated output is not byte-identical to the input")
	}
}
