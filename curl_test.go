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

// pcrPacket is a video packet whose adaptation field carries a PCR at `sec`
// seconds -- the clock deliver's trim measures footage age with. Keyframes in
// these fixtures always ride PCR packets, like a real encoder's GOP heads.
func pcrPacket(cc byte, key bool, seq int, sec float64) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = 0x47
	p[1] = 0x40 | vidHi
	p[2] = vidLo
	p[3] = 0x30 | cc
	p[4] = 7    // adaptation_field_length: flags + 6 PCR bytes
	p[5] = 0x10 // PCR_flag
	if key {
		p[5] |= 0x40 // random_access_indicator
	}
	base := uint64(sec * 90000)
	p[6] = byte(base >> 25)
	p[7] = byte(base >> 17)
	p[8] = byte(base >> 9)
	p[9] = byte(base >> 1)
	p[10] = byte(base<<7) | 0x7e // low bit of base, reserved bits set
	p[11] = 0
	fill(p, 12, seq)
	return p
}

// burstReader hands over `burst` at memory speed, then serves `tail` one
// packet at a time with a real wait before each -- the arrival shape of an
// encoder's buffered backlog followed by live delivery, which is what
// deliver's burst-end detection keys on.
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

// The promise the trim makes: the output starts on a keyframe from INSIDE the
// kept window -- new-channel footage with a cushion behind it -- and nothing
// older survives, however many keyframes the pre-gate footage carried.
func TestDeliverTrimsPreGateFootage(t *testing.T) {
	var input []byte
	input = append(input, patPacket(0)...)
	input = append(input, pmtPacket(0)...)
	const n = 2000 // 5s of PCR time
	for i := 0; i < n; i++ {
		sec := float64(i) * 5.0 / n
		key := i > 0 && i%250 == 0
		if i%10 == 0 {
			input = append(input, pcrPacket(byte(i&0x0f), key, i+1, sec)...)
		} else {
			input = append(input, videoPacket(byte(i&0x0f), false, i+1)...)
		}
	}
	var tail []byte
	for i := 0; i < 40; i++ {
		sec := 5.0 + float64(i)*0.002
		if i%10 == 0 {
			tail = append(tail, pcrPacket(byte(i&0x0f), false, n+i+1, sec)...)
		} else {
			tail = append(tail, videoPacket(byte(i&0x0f), false, n+i+1)...)
		}
	}
	full := append(append([]byte{}, input...), tail...)

	var buf bytes.Buffer
	saved := stdoutW
	stdoutW = &buf
	defer func() { stdoutW = saved }()

	wrote, err := deliver(deliverConfig(), &burstReader{burst: input, tail: tail}, true)
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
	startPCR, ok := firstPCRAt(full, idx)
	if !ok {
		t.Fatal("no PCR at or after the start point")
	}
	// The start must be the NEWEST safe keyframe: in this fixture keyframes are
	// 0.625s apart, so the distance behind live can never legitimately reach a
	// full spacing plus the tail -- and it must still be new-channel footage,
	// inside the safe window.
	age := 5.0 - startPCR
	if age < 0 || age > 0.9 {
		t.Errorf("start is %.2fs behind the live edge; want the newest clean keyframe (under one 0.625s GOP)", age)
	}
	if age > (cushionBuild - renderMargin).Seconds() {
		t.Errorf("start is %.2fs old, outside the %.2fs safe window", age, (cushionBuild - renderMargin).Seconds())
	}
}

// Without a PCR clock the burst cannot be vetted, and the safe direction is a
// clean start: every pre-gate byte is discarded -- keyframes and all -- and
// alignment happens on the live stream.
func TestDeliverNoPCRStartsFromLive(t *testing.T) {
	backlog := buildStream(200, 10)
	var tail []byte
	for i := 0; i < 60; i++ {
		tail = append(tail, videoPacket(byte(i&0x0f), i == 20, 500+i)...)
	}
	full := append(append([]byte{}, backlog...), tail...)

	var buf bytes.Buffer
	saved := stdoutW
	stdoutW = &buf
	defer func() { stdoutW = saved }()

	wrote, err := deliver(deliverConfig(), &burstReader{burst: backlog, tail: tail}, true)
	if !wrote || !errors.Is(err, io.EOF) {
		t.Fatalf("deliver = %v, %v; want wrote with the stream ending", wrote, err)
	}
	out := buf.Bytes()
	rest := out[2*tsPacketSize:]
	idx := bytes.Index(full, rest)
	if idx < 0 || idx%tsPacketSize != 0 || idx+len(rest) != len(full) {
		t.Fatal("output is not a contiguous run of the input to its end")
	}
	if idx < len(backlog) {
		t.Errorf("output starts %d bytes into the unvetted backlog; nothing pre-gate may survive without a clock to trim by", len(backlog)-idx)
	}
	if !randomAccess(rest[:tsPacketSize]) {
		t.Error("output does not start on a keyframe")
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

	wrote, err := deliver(deliverConfig(), bytes.NewReader(input), false)
	if !wrote || !errors.Is(err, io.EOF) {
		t.Fatalf("deliver = %v, %v; want wrote with the stream ending", wrote, err)
	}
	if !bytes.Equal(buf.Bytes(), input) {
		t.Error("ungated output is not byte-identical to the input")
	}
}
