// streamgate -- ah4c CMDn helper.
//
// Waits for the Android box to actually be playing video, then streams the
// encoder through untouched. Same job as streamgate.sh, but as a single process
// so it can do three things the shell cannot:
//
//  1. Hold the encoder connection open DURING the detection wait, so the handoff
//     costs nothing. (exec curl costs ~21ms; that is small, but it is also the
//     only part of the shell design that had to be paid at the worst moment.)
//
//  2. Start the output ON a keyframe, prefixed with the cached PAT/PMT. curl
//     attaches wherever the encoder happens to be in its GOP, so the DVR gets a
//     mid-GOP fragment with no program tables and cannot decode until the next
//     keyframe AND the next PSI cycle both arrive.
//
//  3. Never emit a byte from before the gate opened. It only ever discards
//     forward. It is structurally incapable of introducing a channel-change
//     banner, a tuning prompt, or a blue flash that curl would not also have
//     delivered -- because everything it emits, curl would have emitted too.
//
// stdout is the video stream. Nothing but stream bytes may ever be written
// there; all logging goes to stderr.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// A read that returns faster than this came out of a buffer rather than off
	// the network. 500us is far above the few microseconds a buffered copy takes
	// and far below the milliseconds between packets at any realistic bitrate.
	// DRAIN_IDLE=0 turns catching up off entirely.
	maxDrain = 20000 // ~3.7MB, a stop so a bursting encoder cannot spin

	tsPacketSize = 188
	// After ALIGN_TIMEOUT we stop insisting on motion and take any keyframe;
	// this is the extra grace before giving up on alignment entirely.
	relaxWindow = 2 * time.Second
)

type config struct {
	tuner         string
	tunerIP       string
	encoderURL    string
	minWait       time.Duration
	tuneTimeout   time.Duration
	confirm       int
	poll          time.Duration
	confirmPoll   time.Duration
	settle        time.Duration
	onTimeout     string
	alignKey      bool
	alignTimeout  time.Duration
	waitMotion    bool
	motionTimeout time.Duration
	riseFactor    float64
	motionHold    int
	motionWindow  time.Duration
	rearmMotion   bool
	drainIdle     time.Duration
	readTimeout   time.Duration
	waitAudio     bool
	renderTimeout time.Duration
	debug         bool
}

// stdoutW is the stream sink. A var only so tests can substitute a buffer --
// nothing but stream bytes may ever be written here.
var stdoutW io.Writer = os.Stdout

func logf(c *config, format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "streamgate[%s]: %s\n", c.tuner, fmt.Sprintf(format, a...))
}

// envRaw trims whitespace and strips surrounding quotes. Docker --env-file does
// not strip quotes and env files routinely carry trailing spaces, so a value
// written as ON_TIMEOUT="fail" arrives with the quotes attached. Comparing that
// literally silently disabled the fail-safe.
func envRaw(k string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if len(v) >= 2 {
		q := v[0]
		if (q == 0x22 || q == 0x27) && v[len(v)-1] == q {
			v = v[1 : len(v)-1]
		}
	}
	return strings.TrimSpace(v)
}

func env(k, def string) string {
	if v := envRaw(k); v != "" {
		return v
	}
	return def
}

// envBool accepts the usual spellings instead of "anything except 0".
func envBool(k string, def bool) bool {
	switch strings.ToLower(envRaw(k)) {
	case "":
		return def
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// envDur accepts a bare number of seconds ("5", "0.25") or Go duration syntax
// ("10s", "250ms"). Users of a Go program reasonably type the latter, and
// silently falling back to the default made misconfiguration invisible.
func envDur(k string, def time.Duration) time.Duration {
	v := envRaw(k)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			warnEnv(k, v, "must be positive")
			return def
		}
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		warnEnv(k, v, "want seconds (5, 0.25) or a duration (10s, 250ms)")
		return def
	}
	return time.Duration(f * float64(time.Second))
}

func envFloat(k string, def float64) float64 {
	v := envRaw(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		warnEnv(k, v, "want a positive number")
		return def
	}
	return f
}

func envInt(k string, def int) int {
	v := envRaw(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		warnEnv(k, v, "want a non-negative whole number")
		return def
	}
	return n
}

// warnEnv makes a bad setting visible rather than silently using the default.
func warnEnv(k, v, why string) {
	fmt.Fprintf(os.Stderr, "streamgate: ignoring %s=%q -- %s; using the default\n", k, v, why)
}

func loadConfig() (*config, error) {
	if len(os.Args) < 2 {
		return nil, errors.New("usage: streamgate <tuner-number>")
	}
	n := os.Args[1]
	c := &config{
		tuner: n,
		// Through envRaw like everything else. docker --env-file does not strip
		// quotes, and a TUNER1_IP="10.0.0.5:5555" written that way used to reach
		// adb with the quotes attached, so every poll failed and every tune sat
		// out the full TUNE_TIMEOUT while the log blamed the port.
		tunerIP:     envRaw("TUNER" + n + "_IP"),
		encoderURL:  envRaw("ENCODER" + n + "_URL"),
		minWait:     envDur("MIN_WAIT", 1*time.Second),
		tuneTimeout: envDur("TUNE_TIMEOUT", 40*time.Second),
		confirm:     envInt("CONFIRM", 1),
		poll:        envDur("POLL", 250*time.Millisecond),
		confirmPoll: envDur("CONFIRM_POLL", 50*time.Millisecond),
		// The decoder is allocated a beat before the display catches up, so a
		// short settle here keeps keyframe alignment from landing on pre-render
		// frames and showing a brief blue/black flash at the head of a recording.
		// Far cheaper than waiting for the render signal proper (~0.7s).
		settle:        envDur("SETTLE", 250*time.Millisecond),
		onTimeout:     strings.ToLower(env("ON_TIMEOUT", "fail")),
		alignKey:      envBool("ALIGN_KEYFRAME", true),
		alignTimeout:  envDur("ALIGN_TIMEOUT", 8*time.Second),
		waitMotion:    envBool("WAIT_MOTION", true),
		motionTimeout: envDur("MOTION_TIMEOUT", 6*time.Second),
		riseFactor:    envFloat("RISE_FACTOR", 5.0),
		motionHold:    envInt("MOTION_HOLD", 3),
		motionWindow:  envDur("MOTION_WINDOW", 250*time.Millisecond),
		rearmMotion:   envBool("REARM_MOTION", false),
		drainIdle:     envDur("DRAIN_IDLE", 500*time.Microsecond),
		readTimeout:   envDur("READ_TIMEOUT", 10*time.Second),
		waitAudio:     envBool("WAIT_AUDIO", false),
		renderTimeout: envDur("RENDER_TIMEOUT", 3*time.Second),
		debug:         envBool("DEBUG", false),
	}
	// MOTION_TIMEOUT must expire before the alignment fallback, or raising it
	// silently disables BOTH gates and reports "no keyframe at all", which is a
	// false statement about the encoder.
	if c.motionTimeout >= c.alignTimeout {
		c.alignTimeout = c.motionTimeout + 2*time.Second
	}
	if c.encoderURL == "" {
		return nil, fmt.Errorf("ENCODER%s_URL not set", n)
	}
	return c, nil
}

// ---------------------------------------------------------------- detection

var reID = regexp.MustCompile(`(?m)^\s*Id:\s*(\S+)`)

// secureCodecIDs returns the client ids of every allocated secure video decoder,
// in the order the dump lists them. Android hands out a new client per playback
// session, so an id we have not seen before is the signal -- "a decoder exists"
// is not, since the previous channel's decoder may still be allocated when the
// gate starts.
//
// A set rather than a single id on purpose. An in-place channel switch leaves
// the previous decoder allocated alongside the new one, and picking one of them
// means betting on the order the dump happens to use: pick the old id and the
// gate never fires, pick a pre-existing one from a two-decoder baseline and the
// gate fires immediately on the channel we are trying to leave. Comparing sets
// is correct whatever the order.
func secureCodecIDs(dump string) []string {
	lines := strings.Split(dump, "\n")
	inProc := false
	id := ""
	var found []string
	seen := map[string]bool{}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Process Pid override") || strings.HasPrefix(t, "Events logs") {
			break
		}
		if !inProc {
			if t == "Processes:" {
				inProc = true
			}
			continue
		}
		if m := reID.FindStringSubmatch(line); m != nil {
			id = m[1]
			continue
		}
		l := strings.ToLower(line)
		if strings.Contains(l, "video-codec") || strings.Contains(l, "videocodec") {
			if !strings.Contains(l, "non-secure") && strings.Contains(l, "secure") {
				if id != "" && !seen[id] {
					seen[id] = true
					found = append(found, id)
				}
			}
		}
	}
	return found
}

func setOf(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

// newCodec returns the first id that was not present at baseline, or "".
func newCodec(ids []string, base map[string]bool) string {
	for _, id := range ids {
		if !base[id] {
			return id
		}
	}
	return ""
}

// mediaSessionState reads the top media session. Fallback for devices where the
// resource-manager format above is absent or unfamiliar -- notably anything
// older than Android 11, and vendor builds that report codecs differently.
// Without a fallback such a device never detects anything and every tune fails.
//
// state=3 with speed=1 is playing; state=2 is stopped. Checked in that order
// because a line can carry both and playing is the stronger claim.
func mediaSessionState(dump string) string {
	for _, line := range strings.Split(dump, "\n") {
		if !strings.Contains(line, "PlaybackState") {
			continue
		}
		if (strings.Contains(line, "state=3") || strings.Contains(line, "PLAYING(3)")) &&
			strings.Contains(line, "speed=1") {
			return "playing"
		}
		if strings.Contains(line, "state=2") {
			return "stopped"
		}
		return "unknown"
	}
	return "unknown"
}

// adbCallTimeout is the ceiling on a single adb invocation.
const adbCallTimeout = 10 * time.Second

// adbShell runs one adb command, bounded by whatever is left of the caller's
// budget. Passing the remaining budget matters because TUNE_TIMEOUT is only
// checked at the top of the poll loop: a flat 10s ceiling let a call started
// just under the deadline run the loop ~12s past it.
func adbShell(ctx context.Context, ip, cmd string, budget time.Duration) string {
	if budget <= 0 {
		return ""
	}
	if budget > adbCallTimeout {
		budget = adbCallTimeout
	}
	c, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// WaitDelay matters: without it Output() waits for EOF on pipes that the adb
	// server daemon may have inherited, so it can block well past the context
	// deadline.
	ec := exec.CommandContext(c, "adb", "-s", ip, "shell", cmd)
	ec.WaitDelay = 2 * time.Second
	out, err := ec.Output()
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(out), "\r", "")
}

// waitForVideo blocks until a new secure decoder has been seen `confirm` times.
func waitForVideo(ctx context.Context, c *config) error {
	start := time.Now()
	deadline := start.Add(c.tuneTimeout)

	connect := func() {
		cc, ccCancel := context.WithTimeout(ctx, adbCallTimeout)
		ec := exec.CommandContext(cc, "adb", "connect", c.tunerIP)
		ec.WaitDelay = 2 * time.Second
		_ = ec.Run()
		ccCancel()
	}
	connect()
	lastConnect := time.Now()

	// One round trip per poll: the resource dump, a marker, then the top
	// PlaybackState line.
	const probe = "dumpsys media.resource_manager; echo __MS__; dumpsys media_session | grep -m1 PlaybackState"
	split := func(out string) (string, string) {
		if i := strings.Index(out, "__MS__"); i >= 0 {
			return out[:i], out[i+len("__MS__"):]
		}
		return out, ""
	}

	// The baseline has to come from a probe that actually succeeded, so it is
	// taken by the first good poll rather than by one unchecked call up front.
	// The call immediately after `adb connect` is the one most likely to fail,
	// and an empty baseline makes EVERY decoder look new -- so the previous
	// channel's still-allocated codec opened the gate on the first poll, with
	// the old channel still on screen. Same for the session: an empty dump reads
	// as "not playing", which armed the fallback instantly.
	haveBase := false
	var baseIDs []string
	var baseSet map[string]bool
	armed := false

	hits := 0
	adbFailures, polls, consecFailures := 0, 0, 0
	for {
		elapsed := time.Since(start)
		if elapsed >= c.tuneTimeout {
			// Say WHICH failure this was. adb being unreachable and the box simply
			// not playing produced the same message, which is undiagnosable.
			if polls > 0 && adbFailures == polls {
				return fmt.Errorf("no playback after %ds -- every adb call to %s failed or returned nothing (is adb reachable? does TUNER%s_IP include :5555?)",
					int(elapsed.Seconds()), c.tunerIP, c.tuner)
			}
			if !haveBase {
				return fmt.Errorf("no playback after %ds (adb ok on %d/%d polls) -- never got a usable baseline from %s",
					int(elapsed.Seconds()), polls-adbFailures, polls, c.tunerIP)
			}
			return fmt.Errorf("no playback after %ds (adb ok on %d/%d polls, base=%s) -- device reported no secure decoder and no playing media session",
				int(elapsed.Seconds()), polls-adbFailures, polls, orNone(strings.Join(baseIDs, ",")))
		}
		polls++

		raw := adbShell(ctx, c.tunerIP, probe, time.Until(deadline))
		if raw == "" {
			adbFailures++
			consecFailures++
			// These sessions drop -- the box sleeps, the network blips, adb decides
			// the device is offline. Without this a drop meant every remaining poll
			// failed and the tune sat out the full TUNE_TIMEOUT. Rate limited so ten
			// tuners cannot flood one adb server with reconnects.
			if consecFailures >= 3 && time.Since(lastConnect) > 5*time.Second {
				lastConnect = time.Now()
				connect()
			}
			time.Sleep(c.poll)
			continue
		}
		consecFailures = 0
		rm, ms := split(raw)
		ids := secureCodecIDs(rm)
		session := mediaSessionState(ms)

		if !haveBase {
			haveBase = true
			baseIDs, baseSet = ids, setOf(ids)
			// The session has no identity, so it only counts once it has dropped at
			// least once since we started -- otherwise a session left parked at
			// state=3 by the previous channel reads as instant success.
			armed = session != "playing"
			if c.debug {
				logf(c, "baseline codec=%s session=%s armed=%v (poll %d)",
					orNone(strings.Join(baseIDs, ",")), session, armed, polls)
			}
			time.Sleep(c.poll)
			continue
		}

		// A decoder we did not start with is proof of new playback and needs no
		// arming, because it carries its own identity. The session does not, so it
		// only counts once armed.
		via := ""
		playing := false
		switch {
		case newCodec(ids, baseSet) != "":
			playing, via = true, "codec "+newCodec(ids, baseSet)
		case session == "playing" && armed:
			playing, via = true, "session playing"
		}
		if session != "playing" {
			armed = true
		}

		if c.debug {
			logf(c, "t=%.1fs codec=%s base=%s session=%s armed=%v hits=%d playing=%v",
				elapsed.Seconds(), orNone(strings.Join(ids, ",")),
				orNone(strings.Join(baseIDs, ",")), session, armed, hits, playing)
		}

		if playing && elapsed >= c.minWait {
			hits++
			if hits >= c.confirm {
				if c.settle > 0 {
					time.Sleep(c.settle)
				}
				logf(c, "playback detected after %ds via %s (base %s), %d confirmation(s)",
					int(time.Since(start).Seconds()), via, orNone(strings.Join(baseIDs, ",")), hits)
				return nil
			}
			time.Sleep(c.confirmPoll)
			continue
		}
		hits = 0
		time.Sleep(c.poll)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// audioStarted reports whether the app has an AudioTrack in the started state
// carrying media content.
//
// This is the render signal, and it is why it matters: on a tunneled+secure
// pipeline the video decoder is slaved to the audio hardware sync clock
// (ACodec configures tunneled playback with an audio-hw-sync id), so the first
// pixel cannot be presented before the tunneled audio output is running.
// Decoder ALLOCATION -- which is what media.resource_manager reports -- happens
// measurably earlier, ~0.6-0.8s on the hardware this was written against.
//
// Handing off at allocation used to be harmless because the DVR then discarded
// everything up to the next keyframe, which usually landed after the transition
// had finished. Once the output is keyframe-aligned that slack disappears and
// the pre-render frames become visible, which shows up as a blue/black flash at
// the head of the recording.
func audioStarted(ctx context.Context, c *config, budget time.Duration) bool {
	dump := adbShell(ctx, c.tunerIP, "dumpsys audio", budget)
	if dump == "" {
		return false
	}
	for _, line := range strings.Split(dump, "\n") {
		if !strings.Contains(line, "AudioPlaybackConfiguration") {
			continue
		}
		if !strings.Contains(line, "state:started") {
			continue
		}
		// Ignore UI sounds; only real media counts.
		if strings.Contains(line, "CONTENT_TYPE_MOVIE") ||
			strings.Contains(line, "CONTENT_TYPE_MUSIC") ||
			strings.Contains(line, "usage=USAGE_MEDIA") {
			return true
		}
	}
	return false
}

// waitForRender blocks until audio playback has actually started, bounding the
// wait so a device that never reports it still tunes.
func waitForRender(ctx context.Context, c *config) {
	if !c.waitAudio {
		return
	}
	start := time.Now()
	deadline := start.Add(c.renderTimeout)
	for time.Since(start) < c.renderTimeout {
		if audioStarted(ctx, c, time.Until(deadline)) {
			logf(c, "render confirmed after a further %dms (audio playback started)",
				time.Since(start).Milliseconds())
			return
		}
		time.Sleep(c.confirmPoll)
	}
	logf(c, "render not confirmed within %s, handing off anyway", c.renderTimeout)
}

// ------------------------------------------------------------ TS inspection

// pid returns the 13-bit PID of a TS packet.
func pid(p []byte) uint16 { return (uint16(p[1]&0x1f) << 8) | uint16(p[2]) }

// randomAccess reports whether the adaptation field marks this packet as a
// random access point (the start of a decodable picture).
func randomAccess(p []byte) bool {
	afc := (p[3] >> 4) & 0x03
	if afc != 2 && afc != 3 {
		return false
	}
	afLen := int(p[4])
	if afLen == 0 {
		return false
	}
	return p[5]&0x40 != 0
}

// videoPIDs parses a PMT payload and returns the PIDs carrying video.
// stream_type 0x01,0x02 MPEG-2, 0x1b H.264, 0x24 HEVC.
func videoPIDs(payload []byte) map[uint16]bool {
	out := map[uint16]bool{}
	if len(payload) < 12 {
		return out
	}
	sectionLen := int(uint16(payload[1]&0x0f)<<8 | uint16(payload[2]))
	if sectionLen+3 > len(payload) {
		sectionLen = len(payload) - 3
	}
	progInfoLen := int(uint16(payload[10]&0x0f)<<8 | uint16(payload[11]))
	i := 12 + progInfoLen
	end := 3 + sectionLen - 4 // minus CRC
	for i+4 <= end && i+5 <= len(payload) {
		st := payload[i]
		epid := uint16(payload[i+1]&0x1f)<<8 | uint16(payload[i+2])
		esLen := int(uint16(payload[i+3]&0x0f)<<8 | uint16(payload[i+4]))
		if st == 0x01 || st == 0x02 || st == 0x1b || st == 0x24 {
			out[epid] = true
		}
		i += 5 + esLen
	}
	return out
}

// pmtPIDs parses a PAT payload and returns the PIDs carrying PMTs.
func pmtPIDs(payload []byte) map[uint16]bool {
	out := map[uint16]bool{}
	if len(payload) < 8 {
		return out
	}
	sectionLen := int(uint16(payload[1]&0x0f)<<8 | uint16(payload[2]))
	end := 3 + sectionLen - 4
	for i := 8; i+4 <= end && i+4 <= len(payload); i += 4 {
		prog := uint16(payload[i])<<8 | uint16(payload[i+1])
		p := uint16(payload[i+2]&0x1f)<<8 | uint16(payload[i+3])
		if prog != 0 {
			out[p] = true
		}
	}
	return out
}

// payload returns the payload bytes of a TS packet, skipping the adaptation
// field and (for PSI) the pointer_field.
func payload(p []byte, psi bool) []byte {
	afc := (p[3] >> 4) & 0x03
	off := 4
	if afc == 2 {
		return nil
	}
	if afc == 3 {
		off += 1 + int(p[4])
	}
	if off >= tsPacketSize {
		return nil
	}
	b := p[off:]
	if psi && p[1]&0x40 != 0 { // payload_unit_start
		if len(b) < 1 {
			return nil
		}
		ptr := int(b[0])
		if 1+ptr > len(b) {
			return nil
		}
		b = b[1+ptr:]
	}
	return b
}

// idleTimeoutConn fails a read that goes quiet for too long, instead of blocking
// on it forever.
type idleTimeoutConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Read(b)
}

// ------------------------------------------------------------------ streaming

// stream opens the encoder and copies it to stdout. If the gate has not opened
// yet it discards; once opened it emits PAT/PMT then aligns to the next
// keyframe, so the DVR's first bytes are immediately decodable.
//
// It never emits anything received before `gate` fires. Everything it writes,
// curl would also have written -- it only ever skips forward.
func stream(ctx context.Context, c *config, gate <-chan struct{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.encoderURL, nil)
	if err != nil {
		return err
	}
	// Two different hangs have to be closed off here.
	//
	// ResponseHeaderTimeout: an encoder that accepts the TCP connection but never
	// replies would otherwise park forever, with no log and no exit, while the
	// DVR waits. The dial timeout does not cover it -- the handshake succeeded.
	//
	// The read deadline is the more important one. Without it, an encoder that
	// keeps the connection open but stops sending -- lost HDMI input, a wedged
	// encode thread, a session limit hit mid-stream -- stalls the recording
	// permanently and silently. TCP keepalive does not catch it, because the peer
	// is still answering. Every successful read pushes the deadline out, so this
	// only fires on genuine silence.
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &idleTimeoutConn{Conn: conn, idle: c.readTimeout}, nil
		},
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   1,
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("encoder returned %s", resp.Status)
	}

	wrote := false
	sent := 0
	// 8KB, not 64KB. Whatever sits in this buffer is video we have taken off the
	// socket but not yet emitted, so its size is a ceiling on how far behind live
	// we can be. At a typical 3-4 Mbps, 64KB is ~160ms of held video; 8KB is
	// ~20ms. The cost is more read syscalls -- about 50/sec instead of 6 -- which
	// at ~1us each is nothing.
	br := bufio.NewReaderSize(resp.Body, 1<<13)

	// Coalesce writes without ever holding video back.
	//
	// A size-triggered buffer (the earlier bufio.Writer) is a correctness bug: it
	// waits for the buffer to FILL, so the DVR gets bursts instead of a stream.
	// But one write(2) per 188-byte packet is ~4,650 syscalls/sec per tuner, and
	// if that cannot keep pace with arrival, packets queue in the read buffer and
	// latency grows.
	//
	// So batch only what has ALREADY arrived and flush as soon as nothing more is
	// immediately readable. Latency is identical to writing per packet -- nothing
	// is ever held waiting for more data -- at a fraction of the syscalls.
	// 8 packets, not 64. The batch normally flushes the moment the reader runs
	// dry, so it rarely fills -- but the cap is the worst-case hold time, and at
	// 64 packets that ceiling was ~28ms of video sitting in memory. At 8 it is
	// ~3.5ms, still an 8x cut in syscalls versus writing every packet.
	batch := make([]byte, 0, 8*tsPacketSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := stdoutW.Write(batch)
		sent += n
		batch = batch[:0]
		return err
	}

	var lastPAT, lastPMT []byte
	pmtPids := map[uint16]bool{}
	vidPids := map[uint16]bool{}

	open := false
	aligned := false
	pkt := make([]byte, tsPacketSize)
	discarded := 0
	skippedKeys := 0
	winBytes := 0
	winStart := time.Now()
	floorRate := -1.0
	motionSeen := false
	motionStreak := 0
	var motionAt time.Time
	var motionRate, motionFloor float64
	var gateAt time.Time
	draining := false
	drained := 0
	var readWait time.Duration

	for {
		if !open {
			select {
			case <-gate:
				open = true
				gateAt = time.Now()
				draining = c.drainIdle > 0
				if !c.alignKey {
					aligned = true
				}
				// Opt-in. Everything measured before the gate came off a DIFFERENT
				// picture -- usually the channel we are leaving -- so a rise seen
				// back there can latch motionSeen and wave the new channel's loading
				// card straight through. Re-learning from the gate closes that, but
				// it is off by default because a switch that never shows a card (a
				// retune to what is already playing) then has no rise to find and
				// pays the whole MOTION_TIMEOUT.
				if c.rearmMotion {
					motionSeen, motionStreak = false, 0
					floorRate = -1
					winBytes, winStart = 0, gateAt
				}
			default:
			}
		}

		// Not every encoder signals random access in the adaptation field, and
		// not every stream is one this parser understands. Without this bound a
		// stream that never presents a recognisable keyframe would be discarded
		// forever and the tune would produce no output at all -- far worse than
		// the mid-GOP start we were trying to improve on. Give up and pass the
		// bytes through instead.
		if open && !aligned && time.Since(gateAt) > c.alignTimeout+relaxWindow {
			aligned = true
			logf(c, "no keyframe recognised within %s; streaming unaligned (encoder may not signal random access -- try ALIGN_KEYFRAME=0)", c.alignTimeout+relaxWindow)
			// Still send the tables, otherwise the DVR has to wait for the
			// encoder's next PSI cycle on top of everything else.
			if lastPAT != nil {
				batch = append(batch, lastPAT...)
			}
			if lastPMT != nil {
				batch = append(batch, lastPMT...)
			}
		}

		var readStart time.Time
		if draining {
			readStart = time.Now()
		}
		if err := readPacket(br, pkt); err != nil {
			// Report a failed final flush instead of the read error. If the DVR hung
			// up, that is the real reason we are here and main treats it as the
			// normal end of a recording; blaming the encoder logged a false failure.
			if ferr := flush(); ferr != nil {
				return ferr
			}
			// An encoder with no input can answer 200 with an empty body. Treating
			// that as a clean EOF exited 0 having sent the DVR nothing at all.
			if !wrote {
				return fmt.Errorf("encoder sent no video (%v)", err)
			}
			// The encoder ending the stream mid-recording is a failure however
			// politely it closes. Reporting io.EOF as success truncated recordings
			// with no log line at all.
			return fmt.Errorf("encoder ended the stream after %s and %.1f MB (%v)",
				time.Since(gateAt).Round(time.Second), float64(sent)/(1<<20), err)
		}
		if draining {
			readWait = time.Since(readStart)
		}

		p := pid(pkt)

		// Keep the newest program tables so a keyframe-aligned start is decodable
		// immediately, instead of waiting for the encoder's next PSI cycle.
		if p == 0 {
			lastPAT = append(lastPAT[:0], pkt...)
			if pl := payload(pkt, true); pl != nil {
				if m := pmtPIDs(pl); len(m) > 0 {
					pmtPids = m
				}
			}
		} else if pmtPids[p] {
			lastPMT = append(lastPMT[:0], pkt...)
			if pl := payload(pkt, true); pl != nil {
				if m := videoPIDs(pl); len(m) > 0 {
					vidPids = m
				}
			}
		}

		// Catch up to live before choosing a keyframe.
		//
		// We connect to the encoder early, because the motion gate has to watch the
		// stream to learn what a still picture costs before it can recognise
		// movement. That means the encoder is sending for several seconds while we
		// are still waiting on the box, and anything we have not consumed sits in
		// the socket buffer. Align without clearing it and we lock onto a keyframe
		// that is ALREADY old, then stay exactly that far behind live for the whole
		// recording -- video still arrives, it is just permanently late.
		//
		// So discard whatever was queued, and detect the end of the queue by timing
		// the reads: buffered packets are handed over in microseconds, while at live
		// each read has to wait for the network. The first read that actually waits
		// means the queue is empty and we are current. Bounded by maxDrain so an
		// encoder that genuinely bursts faster than real time cannot spin here.
		//
		// This runs before the motion accounting below on purpose. A drained burst
		// arrives thousands of times faster than real time, and counting it would
		// read as a colossal bitrate spike and trip the motion gate instantly --
		// exactly the loading-screen false positive the gate exists to prevent.
		if draining {
			if readWait < c.drainIdle && drained < maxDrain {
				drained++
				discarded++
				continue
			}
			draining = false
			// Restart the measurement window; the one in progress spans the burst.
			winBytes, winStart = 0, time.Now()
		}

		// Tell a loading screen from programming by how hard the picture is to
		// compress: a static card costs the encoder almost nothing, moving video
		// costs it everything. Nothing here is compared against a fixed bitrate,
		// because absolute numbers are meaningless across encoders, resolutions
		// and quality settings. The stream calibrates itself -- remember the
		// quietest window seen, and call it motion when a window rises well above
		// that floor and HOLDS, since the cut itself produces brief spikes.
		//
		// Only until we align: nothing below reads any of it afterwards, and
		// keeping it off the post-alignment path is what makes it impossible for
		// the long-window reset to drop a packet out of the stream.
		if !aligned {
			// Exclude null padding. A constant-bitrate encoder pads with PID 0x1FFF
			// to hold the mux rate steady, so counting every packet measures the mux
			// rate rather than the picture and motion can never be detected.
			if p != 0x1fff {
				winBytes += tsPacketSize
			}
			if now := time.Now(); now.Sub(winStart) >= c.motionWindow {
				elapsed := now.Sub(winStart)
				// Ignore windows that ran long. If the stream stalls -- an HDMI resync
				// on wake is enough -- the window still closes and measures a near-zero
				// rate, which latches the floor permanently low and makes every later
				// window look like motion.
				//
				// Reset and fall through; do NOT skip the rest of the loop. Continuing
				// here used to drop this packet -- before alignment that cost a whole
				// GOP by skipping a keyframe, and after it the packet was simply gone
				// from the output, which is a continuity break in the recording.
				if elapsed <= 2*c.motionWindow {
					rate := float64(winBytes) / elapsed.Seconds()
					if floorRate < 0 || rate < floorRate {
						floorRate = rate
					}
					if floorRate > 0 && rate > floorRate*c.riseFactor {
						motionStreak++
						if !motionSeen && motionStreak >= c.motionHold {
							motionSeen = true
							motionAt = now
							motionRate, motionFloor = rate, floorRate
						}
					} else {
						motionStreak = 0
					}
				}
				winBytes, winStart = 0, now
			}
		}

		if !open {
			continue
		}

		if !aligned {
			// Prefer a random access point on a PID the PMT identified as video.
			// If no PMT was seen, or it lists stream types this does not know,
			// take any random access point rather than discarding indefinitely --
			// a slightly worse start beats no picture on unfamiliar hardware.
			if !(randomAccess(pkt) && (vidPids[p] || len(vidPids) == 0)) {
				discarded++
				continue
			}

			// HAPPY -- programming already flowing, the next keyframe is a real
			// picture. SAD -- still on the loading screen; taking a keyframe now
			// records the card, so wait for the picture to start moving. Bounded:
			// a constant-bitrate encoder, or a box that was never slept so there
			// is no rise to see, falls through on MOTION_TIMEOUT.
			if c.waitMotion && !motionSeen && time.Since(gateAt) < c.motionTimeout {
				skippedKeys++
				discarded++
				continue
			}
			aligned = true
			var path string
			// heldFor is the only number that says whether the gate cost anything.
			// It is time spent waiting for the picture to start moving, measured from
			// the gate opening. On a box that sleeps between tunes the stream is always
			// static beforehand, so the detector nearly always trips after the gate --
			// including when it trips 200ms later and the tune is effectively instant.
			var heldFor time.Duration
			if motionSeen && motionAt.After(gateAt) {
				heldFor = motionAt.Sub(gateAt)
			}
			switch {
			case !c.waitMotion:
				path = fmt.Sprintf("motion-gate=off keyframes-skipped=%d", skippedKeys)
			case !motionSeen:
				path = fmt.Sprintf("waited-for-motion=%s(timeout, released anyway) keyframes-skipped=%d",
					c.motionTimeout, skippedKeys)
			default:
				path = fmt.Sprintf("waited-for-motion=%dms picture=%.0fkbps still-picture-floor=%.0fkbps ratio=%.1fx keyframes-skipped=%d",
					heldFor.Milliseconds(), motionRate*8/1000, motionFloor*8/1000,
					motionRate/motionFloor, skippedKeys)
			}
			// video-pid  : which PID carried the keyframe we started on
			// discarded  : bytes dropped between the gate opening and that keyframe
			// gate-to-air: total time from the gate opening to the first byte out
			logf(c, "aligned video-pid=%d discarded=%d packets/%.0fKB caught-up=%.0fKB gate-to-air=%.2fs %s",
				p, discarded, float64(discarded*tsPacketSize)/1024,
				float64(drained*tsPacketSize)/1024,
				time.Since(gateAt).Seconds(), path)
			if lastPAT != nil {
				batch = append(batch, lastPAT...)
			}
			if lastPMT != nil {
				batch = append(batch, lastPMT...)
			}
		}

		batch = append(batch, pkt...)
		wrote = true
		// Flush as soon as nothing more has already arrived, or the batch is full.
		// Never waits for data -- latency matches a per-packet write, at a fraction
		// of the syscalls.
		if br.Buffered() < tsPacketSize || len(batch)+tsPacketSize > cap(batch) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

// readPacket reads one 188-byte TS packet, resynchronising on 0x47 if needed.
func readPacket(br *bufio.Reader, pkt []byte) error {
	for {
		if _, err := io.ReadFull(br, pkt[:1]); err != nil {
			return err
		}
		if pkt[0] != 0x47 {
			continue
		}
		if _, err := io.ReadFull(br, pkt[1:]); err != nil {
			return err
		}
		return nil
	}
}

func main() {
	// os.Stdout raises SIGPIPE, which by default kills the process. That made the
	// NORMAL end of every recording -- the DVR disconnecting -- look like a crash
	// (exit 141), skip the write-error path entirely, and log nothing.
	signal.Ignore(syscall.SIGPIPE)

	c, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "streamgate:", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gate := make(chan struct{})
	streamErr := make(chan error, 1)

	// Open the encoder now, during the detection wait, so the handoff costs
	// nothing. Bytes received before the gate opens are discarded, never emitted.
	go func() { streamErr <- stream(ctx, c, gate) }()

	if c.tunerIP == "" {
		logf(c, "TUNER%s_IP not set -- no gate", c.tuner)
		close(gate)
	} else if err := waitForVideo(ctx, c); err != nil {
		// Surface an encoder failure that already happened, rather than blaming
		// the box. Without this the only log is "no playback after 40s" while the
		// real fault was a dead encoder noticed 40 seconds ago.
		select {
		case se := <-streamErr:
			if se != nil {
				logf(c, "encoder failed during detection: %v", se)
			}
		default:
		}
		logf(c, "%v", err)
		if c.onTimeout == "fail" {
			logf(c, "failing the tune rather than streaming whatever is on screen")
			cancel()
			os.Exit(1)
		}
		close(gate)
	} else {
		// The decoder exists, but on a tunneled pipeline nothing is on screen
		// until audio starts. Keyframe alignment removed the slack that used to
		// hide that gap, so confirm render before opening the gate.
		waitForRender(ctx, c)
		close(gate)
	}

	err = <-streamErr
	switch {
	case err == nil, ctx.Err() != nil:
	case errors.Is(err, syscall.EPIPE), errors.Is(err, os.ErrClosed):
		// The DVR hung up. That is how a recording normally ends.
		logf(c, "stream closed by the DVR")
	default:
		logf(c, "stream ended: %v", err)
		os.Exit(1)
	}
}
