package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestEnvDur(t *testing.T) {
	const def = 6 * time.Second
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", def},
		{"5", 5 * time.Second},
		{"0.25", 250 * time.Millisecond},
		{"10s", 10 * time.Second},         // Go duration syntax
		{"250ms", 250 * time.Millisecond}, // Go duration syntax
		{" 5 ", 5 * time.Second},          // whitespace from env files
		{`"5"`, 5 * time.Second},          // quotes survive docker --env-file
		{"abc", def},
		{"NaN", def}, // used to overflow to a huge negative duration
		{"Inf", def},
		{"0", def},  // zero would busy-loop or fail instantly
		{"-1", def}, // negative likewise
	}
	for _, c := range cases {
		os.Setenv("T_DUR", c.in)
		if got := envDur("T_DUR", def); got != c.want {
			t.Errorf("envDur(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	os.Unsetenv("T_DUR")
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true}, {"0", false}, {"false", false}, {"FALSE", false},
		{"no", false}, {"off", false}, {`"0"`, false}, {" 0 ", false},
		{"1", true}, {"true", true},
	}
	for _, c := range cases {
		os.Setenv("T_BOOL", c.in)
		if got := envBool("T_BOOL", true); got != c.want {
			t.Errorf("envBool(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	os.Unsetenv("T_BOOL")
}

// A trailing space or stray quotes used to silently turn the fail-safe OFF,
// which would record a loading screen for the length of a programme.
func TestOnTimeoutFailSafe(t *testing.T) {
	os.Setenv("ENCODER9_URL", "http://x")
	os.Args = []string{"streamgate", "9"}
	for _, in := range []string{"fail", "fail ", `"fail"`, " FAIL "} {
		os.Setenv("ON_TIMEOUT", in)
		c, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if c.onTimeout != "fail" {
			t.Errorf("ON_TIMEOUT=%q gave %q, want the fail-safe to stay on", in, c.onTimeout)
		}
	}
	os.Unsetenv("ON_TIMEOUT")
	os.Unsetenv("ENCODER9_URL")
}

// docker --env-file keeps the quotes. A quoted IP reached adb verbatim, so every
// poll failed and the tune sat out the whole TUNE_TIMEOUT blaming the port.
func TestAddressesAreUnquoted(t *testing.T) {
	os.Setenv("TUNER9_IP", `"192.168.1.5:5555"`)
	os.Setenv("ENCODER9_URL", " http://10.0.0.9/0.ts ")
	os.Args = []string{"streamgate", "9"}
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.tunerIP != "192.168.1.5:5555" {
		t.Errorf("tunerIP = %q, want the quotes stripped", c.tunerIP)
	}
	if c.encoderURL != "http://10.0.0.9/0.ts" {
		t.Errorf("encoderURL = %q, want it trimmed", c.encoderURL)
	}
	os.Unsetenv("TUNER9_IP")
	os.Unsetenv("ENCODER9_URL")
}

// MOTION_TIMEOUT must expire before the alignment fallback, otherwise raising it
// silently disables both gates and blames the encoder in the log.
func TestTimeoutOrdering(t *testing.T) {
	os.Setenv("ENCODER9_URL", "http://x")
	os.Setenv("MOTION_TIMEOUT", "12")
	os.Setenv("ALIGN_TIMEOUT", "8")
	os.Args = []string{"streamgate", "9"}
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.alignTimeout <= c.motionTimeout {
		t.Errorf("alignTimeout %v must exceed motionTimeout %v", c.alignTimeout, c.motionTimeout)
	}
	os.Unsetenv("MOTION_TIMEOUT")
	os.Unsetenv("ALIGN_TIMEOUT")
	os.Unsetenv("ENCODER9_URL")
}

// ---------------------------------------------------------------- detection

// An in-place channel switch leaves the previous decoder allocated alongside the
// new one. Reporting a single id meant betting on the order the dump lists them;
// both must come back so the caller can compare sets.
func TestSecureCodecIDsReturnsAll(t *testing.T) {
	dump := `
  Processes:
    Pid: 1000
      Id: OLDCODEC
      {name: secure-codec, subType: video-codec, value: 1}
    Pid: 2000
      Id: NEWCODEC
      {name: secure-codec, subType: video-codec, value: 1}
`
	got := secureCodecIDs(dump)
	if len(got) != 2 || got[0] != "OLDCODEC" || got[1] != "NEWCODEC" {
		t.Fatalf("secureCodecIDs = %v, want [OLDCODEC NEWCODEC]", got)
	}
	base := setOf([]string{"OLDCODEC"})
	if id := newCodec(got, base); id != "NEWCODEC" {
		t.Errorf("newCodec = %q, want NEWCODEC", id)
	}
	// Whatever order the device lists them in, the answer is the same.
	if id := newCodec([]string{"NEWCODEC", "OLDCODEC"}, base); id != "NEWCODEC" {
		t.Errorf("newCodec (reversed) = %q, want NEWCODEC", id)
	}
	// Two decoders already allocated at baseline must not read as new.
	if id := newCodec(got, setOf(got)); id != "" {
		t.Errorf("newCodec against its own baseline = %q, want empty", id)
	}
}

func TestSecureCodecIDsRealFormat(t *testing.T) {
	dump := `
  Processes:
    Pid: 4529
      Id: 1284494944
      {name: secure-codec, subType: video-codec, value: 1}
`
	got := secureCodecIDs(dump)
	if len(got) != 1 || got[0] != "1284494944" {
		t.Errorf("secureCodecIDs = %v, want [1284494944]", got)
	}
}

// A non-DRM app allocates a NON-secure decoder; it must not be mistaken for one.
func TestSecureCodecIDsIgnoresNonSecure(t *testing.T) {
	dump := `
  Processes:
    Pid: 1000
      Id: PLAIN
      {name: non-secure-codec, subType: video-codec, value: 1}
`
	if got := secureCodecIDs(dump); len(got) != 0 {
		t.Errorf("secureCodecIDs = %v, want none for a non-secure decoder", got)
	}
}

// History must not be mined for decoders that are no longer allocated.
func TestSecureCodecIDsStopsAtEventsLog(t *testing.T) {
	dump := `
  Processes:
    Pid: 1000
      Id: LIVE
      {name: secure-codec, subType: video-codec, value: 1}
  Events logs:
    Pid: 9999
      Id: HISTORICAL
      {name: secure-codec, subType: video-codec, value: 1}
`
	got := secureCodecIDs(dump)
	if len(got) != 1 || got[0] != "LIVE" {
		t.Errorf("secureCodecIDs = %v, want [LIVE]", got)
	}
}

func TestMediaSessionState(t *testing.T) {
	cases := []struct{ in, want string }{
		{"PlaybackState {state=3, position=0, speed=1.0}", "playing"},
		{"PlaybackState {state=2, position=0, speed=1.0}", "stopped"},
		{"PlaybackState {state=0, position=0, speed=1.0}", "unknown"},
		{"PlaybackState {state=3, position=0, speed=0.0}", "unknown"}, // paused
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := mediaSessionState(c.in); got != c.want {
			t.Errorf("mediaSessionState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ------------------------------------------------------------ TS inspection

// A malformed stream must never panic: a panic in the stream goroutine kills a
// live recording.
func TestParsersDoNotPanic(t *testing.T) {
	pkt := make([]byte, tsPacketSize)
	seed := uint32(12345)
	for i := 0; i < 200000; i++ {
		for j := range pkt {
			seed = seed*1664525 + 1013904223
			pkt[j] = byte(seed >> 16)
		}
		pkt[0] = 0x47
		_ = pid(pkt)
		_ = randomAccess(pkt)
		if pl := payload(pkt, true); pl != nil {
			_ = pmtPIDs(pl)
			_ = videoPIDs(pl)
		}
	}
}

// ------------------------------------------------------------------ streaming

const (
	testPMTPid   uint16 = 0x1000
	testVideoPid uint16 = 0x0100
)

// Split out because byte(testPMTPid) is a constant conversion and 0x1000 does
// not fit in a byte.
const (
	pmtHi, pmtLo = byte(testPMTPid >> 8), byte(testPMTPid & 0xff)
	vidHi, vidLo = byte(testVideoPid >> 8), byte(testVideoPid & 0xff)
)

// fill writes a unique body. The first two bytes are a 16-bit counter: a single
// byte repeated every 256 packets, which silently broke the uniqueness that
// locating a run of output in the input depends on.
func fill(pkt []byte, from int, seq int) {
	for i := from; i < tsPacketSize; i++ {
		pkt[i] = byte(seq*7 + i*13)
	}
	if from+1 < tsPacketSize {
		pkt[from], pkt[from+1] = byte(seq>>8), byte(seq)
	}
}

func patPacket(cc byte) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = 0x47
	p[1] = 0x40 // payload_unit_start, pid 0
	p[2] = 0x00
	p[3] = 0x10 | cc // payload only
	p[4] = 0x00      // pointer_field
	s := p[5:]
	s[0] = 0x00 // table_id
	s[1] = 0xb0
	s[2] = 0x0d // section_length 13
	s[3], s[4] = 0x00, 0x01
	s[5], s[6], s[7] = 0xc1, 0x00, 0x00
	s[8], s[9] = 0x00, 0x01          // program_number 1
	s[10], s[11] = 0xe0|pmtHi, pmtLo // PMT pid
	return p
}

func pmtPacket(cc byte) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = 0x47
	p[1] = 0x40 | pmtHi
	p[2] = pmtLo
	p[3] = 0x10 | cc
	p[4] = 0x00
	s := p[5:]
	s[0] = 0x02 // table_id
	s[1] = 0xb0
	s[2] = 0x12 // section_length 18
	s[3], s[4] = 0x00, 0x01
	s[5], s[6], s[7] = 0xc1, 0x00, 0x00
	s[8], s[9] = 0xe0|vidHi, vidLo   // PCR pid
	s[10], s[11] = 0xf0, 0x00        // program_info_length 0
	s[12] = 0x1b                     // H.264
	s[13], s[14] = 0xe0|vidHi, vidLo // elementary pid
	s[15], s[16] = 0xf0, 0x00        // ES_info_length 0
	return p
}

func videoPacket(cc byte, key bool, seq int) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = 0x47
	p[1] = 0x40 | vidHi
	p[2] = vidLo
	if key {
		p[3] = 0x30 | cc // adaptation field + payload
		p[4] = 0x01      // adaptation_field_length
		p[5] = 0x40      // random_access_indicator
		fill(p, 6, seq)
		return p
	}
	p[3] = 0x10 | cc
	fill(p, 4, seq)
	return p
}

// buildStream returns a TS stream: tables, then video with a keyframe every
// `keyEvery` packets. Every packet body is unique, so a run of output can be
// located unambiguously in the input.
func buildStream(nVideo, keyEvery int) []byte {
	var b []byte
	b = append(b, patPacket(0)...)
	b = append(b, pmtPacket(0)...)
	for i := 0; i < nVideo; i++ {
		b = append(b, videoPacket(byte(i&0x0f), i > 0 && i%keyEvery == 0, i+1)...)
	}
	return b
}

// serveTS writes the stream one packet at a time with a pause between each, so
// reads come off the network rather than out of a buffer.
func serveTS(t *testing.T, data []byte, gap time.Duration) *httptest.Server {
	t.Helper()
	return serveTSGate(t, data, gap, -1, nil)
}

// serveTSGate is serveTS, but it closes `gate` just before writing packet
// gateAt and pauses so the reader is certain to observe it first. That makes the
// gate position KNOWN, which is what lets a test assert the exact packet the
// output started on -- without it, the only start-side assertion possible is
// "not packet zero", which a build that ignores the gate entirely still passes.
func serveTSGate(t *testing.T, data []byte, gap time.Duration, gateAt int, gate chan struct{}) *httptest.Server {
	t.Helper()
	var once sync.Once
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i, n := 0, 0; i+tsPacketSize <= len(data); i, n = i+tsPacketSize, n+1 {
			if gate != nil && n == gateAt {
				once.Do(func() { close(gate) })
				time.Sleep(4 * gap)
			}
			if _, err := w.Write(data[i : i+tsPacketSize]); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(gap)
		}
	}))
}

// firstKeyframeAt returns the index of the first video random-access packet at or
// after `from`, which is exactly where an aligned stream must start.
func firstKeyframeAt(input []byte, from int) int {
	for i := from; (i+1)*tsPacketSize <= len(input); i++ {
		p := input[i*tsPacketSize : (i+1)*tsPacketSize]
		if randomAccess(p) && pid(p) == testVideoPid {
			return i
		}
	}
	return -1
}

func testConfig(url string) *config {
	return &config{
		tuner:         "T",
		encoderURL:    url,
		alignKey:      true,
		alignTimeout:  5 * time.Second,
		waitMotion:    false,
		motionTimeout: time.Second,
		riseFactor:    5,
		motionHold:    3,
		// Deliberately tiny relative to the packet spacing below, so every
		// measurement window closes "long". That is the condition under which the
		// window reset used to skip the rest of the loop and drop the packet.
		motionWindow: time.Millisecond,
		drainIdle:    0, // the test server is not bursting; nothing to catch up on
		readTimeout:  2 * time.Second,
	}
}

// runStream captures what stream() writes to stdout.
func runStream(t *testing.T, c *config, gate <-chan struct{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	saved := stdoutW
	stdoutW = &buf
	defer func() { stdoutW = saved }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The stream always ends in an error -- the encoder stops sending. What
	// matters is what reached stdout before that.
	_ = stream(ctx, c, gate)
	return buf.Bytes()
}

// checkContiguous asserts the README's actual promise: after the injected
// tables, the output is an unbroken run of the input that starts on a keyframe
// and continues to the end. A single dropped packet fails this.
func checkContiguous(t *testing.T, input, out []byte, wantStart int) {
	t.Helper()
	if len(out) < 3*tsPacketSize {
		t.Fatalf("output is %d bytes, want tables plus video", len(out))
	}
	if got := pid(out[0:tsPacketSize]); got != 0 {
		t.Errorf("first packet pid = %d, want the cached PAT (0)", got)
	}
	if got := pid(out[tsPacketSize : 2*tsPacketSize]); got != testPMTPid {
		t.Errorf("second packet pid = %d, want the cached PMT (%d)", got, testPMTPid)
	}
	rest := out[2*tsPacketSize:]
	if !randomAccess(rest[:tsPacketSize]) {
		t.Error("stream did not start on a keyframe")
	}
	idx := bytes.Index(input, rest)
	if idx < 0 {
		t.Fatal("output is not a contiguous run of the input -- a packet was dropped or reordered")
	}
	if idx%tsPacketSize != 0 {
		t.Fatalf("output starts %d bytes into a packet, want packet alignment", idx%tsPacketSize)
	}
	if idx+len(rest) != len(input) {
		t.Fatalf("output ends %d bytes early -- packets were dropped near the end",
			len(input)-(idx+len(rest)))
	}
	// The exact packet, not "somewhere after zero". buildStream never makes
	// packet 0 a keyframe, so idx != 0 held even for a build that ignored the
	// gate completely and emitted 30 packets of pre-gate video -- and equally for
	// one that aligned two keyframes late and threw away good video.
	if wantStart >= 0 && idx/tsPacketSize != wantStart {
		t.Errorf("output starts at packet %d, want %d (the first keyframe at or after the gate)",
			idx/tsPacketSize, wantStart)
	}
}

// The gate is already open, so the only thing between input and output is
// keyframe alignment.
func TestStreamOutputIsContiguous(t *testing.T) {
	input := buildStream(60, 10)
	srv := serveTS(t, input, 3*time.Millisecond)
	defer srv.Close()

	gate := make(chan struct{})
	close(gate)
	checkContiguous(t, input, runStream(t, testConfig(srv.URL), gate), firstKeyframeAt(input, 0))
}

// Nothing received before the gate opens may ever be emitted, and what is
// emitted still has to be unbroken.
func TestStreamEmitsNothingBeforeTheGate(t *testing.T) {
	const gateAt = 40
	input := buildStream(120, 10)
	gate := make(chan struct{})
	srv := serveTSGate(t, input, 3*time.Millisecond, gateAt, gate)
	defer srv.Close()

	want := firstKeyframeAt(input, gateAt)
	if want <= gateAt {
		t.Fatalf("fixture is wrong: no keyframe after the gate at packet %d", gateAt)
	}
	checkContiguous(t, input, runStream(t, testConfig(srv.URL), gate), want)
}

// With ALIGN_KEYFRAME off there is no alignment and no injected tables, but the
// output must still be an unbroken run from wherever the gate opened.
func TestStreamUnalignedIsContiguous(t *testing.T) {
	input := buildStream(60, 10)
	srv := serveTS(t, input, 3*time.Millisecond)
	defer srv.Close()

	c := testConfig(srv.URL)
	c.alignKey = false
	gate := make(chan struct{})
	close(gate)

	out := runStream(t, c, gate)
	if len(out) < tsPacketSize {
		t.Fatalf("output is %d bytes, want video", len(out))
	}
	idx := bytes.Index(input, out)
	if idx < 0 || idx%tsPacketSize != 0 || idx+len(out) != len(input) {
		t.Fatal("unaligned output is not a contiguous run of the input")
	}
}

// stallWriter blocks on one write, the way a DVR stalling on disk or network
// does. Whatever it holds up delays the next measurement window close.
type stallWriter struct {
	buf   bytes.Buffer
	n     int
	after int
	pause time.Duration
}

func (w *stallWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n == w.after {
		time.Sleep(w.pause)
	}
	return w.buf.Write(p)
}

// The realistic form of the dropped-packet bug: DEFAULT MOTION_WINDOW, and the
// stall comes from the DVR rather than from a contrived window size. The other
// stream tests use a 1ms window, which drops every packet -- useful, but not what
// production looked like.
func TestStreamSurvivesADVRStall(t *testing.T) {
	input := buildStream(400, 40)
	srv := serveTS(t, input, 2*time.Millisecond)
	defer srv.Close()

	c := testConfig(srv.URL)
	c.motionWindow = 250 * time.Millisecond // the shipped default
	c.readTimeout = 5 * time.Second

	w := &stallWriter{after: 3, pause: 600 * time.Millisecond}
	gate := make(chan struct{})
	close(gate)

	saved := stdoutW
	stdoutW = w
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = stream(ctx, c, gate)
	cancel()
	stdoutW = saved

	checkContiguous(t, input, w.buf.Bytes(), firstKeyframeAt(input, 0))
}

// A PSI continuation packet carries no table header. Parsing one yields garbage
// PIDs that pass the len(m) > 0 guard, and caching one injects a mid-section
// fragment as the DVR's first table.
func TestStreamIgnoresPSIContinuation(t *testing.T) {
	var input []byte
	input = append(input, patPacket(0)...)
	input = append(input, pmtPacket(0)...)
	for i := 0; i < 5; i++ {
		input = append(input, videoPacket(byte(i), false, i+1)...)
	}
	// payload_unit_start CLEAR with a body that parses to nonsense, on BOTH PSI
	// pids: the PAT and the PMT are cached and parsed by separate branches, and a
	// fixture with only one of them leaves the other branch untested.
	patCont := make([]byte, tsPacketSize)
	patCont[0], patCont[1], patCont[2], patCont[3] = 0x47, 0x00, 0x00, 0x11
	fill(patCont, 4, 0x55)
	input = append(input, patCont...)
	pmtCont := make([]byte, tsPacketSize)
	pmtCont[0], pmtCont[1], pmtCont[2], pmtCont[3] = 0x47, pmtHi, pmtLo, 0x11
	fill(pmtCont, 4, 0x66)
	input = append(input, pmtCont...)
	for i := 5; i < 40; i++ {
		input = append(input, videoPacket(byte(i&0x0f), i%10 == 0, i+1)...)
	}

	srv := serveTS(t, input, 3*time.Millisecond)
	defer srv.Close()
	gate := make(chan struct{})
	close(gate)

	out := runStream(t, testConfig(srv.URL), gate)
	if len(out) < 2*tsPacketSize {
		t.Fatalf("output is %d bytes, want the tables plus video", len(out))
	}
	if !bytes.Equal(out[:tsPacketSize], input[:tsPacketSize]) {
		t.Error("injected PAT is not the real PAT -- a continuation packet was cached as a table")
	}
	if !bytes.Equal(out[tsPacketSize:2*tsPacketSize], input[tsPacketSize:2*tsPacketSize]) {
		t.Error("injected PMT is not the real PMT")
	}
	checkContiguous(t, input, out, firstKeyframeAt(input, 0))
}

// The fail-safe that matters most: a tune that carried no picture must put zero
// bytes on the wire. The ALIGN_TIMEOUT fallback stages the cached tables into the
// batch without setting `wrote`, and flushing those before the !wrote check put
// 376 bytes out on a tune that then reported it had sent no video.
func TestNoVideoMeansNoBytes(t *testing.T) {
	var input []byte
	input = append(input, patPacket(0)...)
	input = append(input, pmtPacket(0)...)
	for i := 0; i < 20; i++ { // video, but never a keyframe
		input = append(input, videoPacket(byte(i&0x0f), false, i+1)...)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		send := func(b []byte) {
			if _, err := w.Write(b); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		send(input)
		// Quiet past alignTimeout+relaxWindow, then one more packet so the loop
		// wakes, trips the fallback, and immediately hits EOF.
		time.Sleep(2400 * time.Millisecond)
		send(videoPacket(1, false, 99))
	}))
	defer srv.Close()

	c := testConfig(srv.URL)
	c.alignTimeout = 100 * time.Millisecond
	c.readTimeout = 6 * time.Second
	gate := make(chan struct{})
	close(gate)

	out := runStream(t, c, gate)
	if len(out) != 0 {
		t.Errorf("stream put %d bytes on stdout for a tune with no decodable video; want 0", len(out))
	}
}

// SETTLE, MIN_WAIT and DRAIN_IDLE document 0 as "off" and branch on it, so 0 has
// to survive parsing. A poll interval still must not be zero.
func TestZeroWhereItIsDocumented(t *testing.T) {
	os.Setenv("ENCODER9_URL", "http://x")
	os.Setenv("MIN_WAIT", "0")
	os.Setenv("SETTLE", "0")
	os.Setenv("DRAIN_IDLE", "0")
	os.Setenv("POLL", "0")
	os.Args = []string{"streamgate", "9"}
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.minWait != 0 || c.settle != 0 || c.drainIdle != 0 {
		t.Errorf("minWait=%v settle=%v drainIdle=%v, want all zero", c.minWait, c.settle, c.drainIdle)
	}
	if c.poll == 0 {
		t.Error("POLL=0 must be rejected -- it would busy-loop")
	}
	for _, k := range []string{"MIN_WAIT", "SETTLE", "DRAIN_IDLE", "POLL", "ENCODER9_URL"} {
		os.Unsetenv(k)
	}
}

// The parsers are fed 200k random packets by TestParsersDoNotPanic, which
// asserts nothing about what comes back. These pin the two bounds that were
// wrong, on the malformed shapes that fuzz was already producing blind.
func TestParserBounds(t *testing.T) {
	// A PMT whose ES loop is followed by a four-byte remainder before the CRC.
	// With the loop bounded at i+4 <= end, the entry at that boundary reads its
	// length field out of the CRC and invents an elementary stream.
	pmt := make([]byte, 60)
	pmt[0] = 0x02
	pmt[1], pmt[2] = 0xb0, 22 // section_length: 9 header + 5 ES + 4 filler + 4 CRC
	pmt[3], pmt[4] = 0x00, 0x01
	pmt[5], pmt[6], pmt[7] = 0xc1, 0x00, 0x00
	pmt[8], pmt[9] = 0xe0|vidHi, vidLo // PCR pid
	pmt[10], pmt[11] = 0xf0, 0x00      // program_info_length 0
	pmt[12] = 0x1b                     // the one real elementary stream
	pmt[13], pmt[14] = 0xe0|vidHi, vidLo
	pmt[15], pmt[16] = 0xf0, 0x00
	// Four bytes of remainder that read as a plausible H.264 entry on pid 0x0099.
	pmt[17] = 0x1b
	pmt[18], pmt[19] = 0xe0, 0x99
	pmt[20] = 0xf0

	got := videoPIDs(pmt)
	if !got[testVideoPid] {
		t.Errorf("videoPIDs lost the real video pid: %v", got)
	}
	if got[0x0099] {
		t.Error("videoPIDs invented a pid from the section remainder and the CRC")
	}
	if len(got) != 1 {
		t.Errorf("videoPIDs = %v, want exactly one pid", got)
	}

	// afc 0 is reserved: the packet carries no payload at all.
	pkt := make([]byte, tsPacketSize)
	pkt[0], pkt[1], pkt[2], pkt[3] = 0x47, 0x40, 0x00, 0x00
	if pl := payload(pkt, true); pl != nil {
		t.Errorf("payload() returned %d bytes for afc=0, want nil", len(pl))
	}
	// afc 2 is adaptation-field-only; same answer, and it already worked.
	pkt[3] = 0x20
	if pl := payload(pkt, true); pl != nil {
		t.Errorf("payload() returned %d bytes for afc=2, want nil", len(pl))
	}
}

// rmWhole is the whole defence against a truncated resource dump becoming the
// codec baseline, and replacing its body with `return true` used to leave the
// entire suite green.
func TestRMWhole(t *testing.T) {
	const whole = "Processes:\n  Pid: 1\n    Id: A\n    {name: secure-codec, subType: video-codec, value: 1}\nEvents logs:\n"
	cases := []struct {
		name         string
		rm           string
		ids          []string
		noTerminator bool
		want         bool
	}{
		{"complete with a decoder", whole, []string{"A"}, false, true},
		{"complete and idle", "Processes:\nEvents logs:\n", nil, false, true},
		{"cut before any decoder", "Processes:\n  Pid: 1\n    Id:", nil, false, false},
		// The dangerous shape: enough arrived to look parsed, so decoder #2 would
		// read as new and open the gate on the channel being left.
		{"cut after the first decoder", "Processes:\n  Pid: 1\n    Id: A\n    {name: secure-codec, subType: video-codec, value: 1}\n  Pid: 2\n    Id: B", []string{"A"}, false, false},
		// ...unless this build has shown us it never prints a terminator, where
		// refusing everything would kill the codec signal outright.
		{"same, on a build with no terminator", "Processes:\n  Pid: 1\n    Id: A\n    {name: secure-codec, subType: video-codec, value: 1}\n  Pid: 2\n    Id: B", []string{"A"}, true, true},
		{"idle, no terminator, relaxed", "Processes:\n", nil, true, false},
	}
	for _, c := range cases {
		if got := rmWhole(c.rm, c.ids, c.noTerminator); got != c.want {
			t.Errorf("%s: rmWhole = %v, want %v", c.name, got, c.want)
		}
	}
}
