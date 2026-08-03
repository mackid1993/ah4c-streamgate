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
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	tsPacketSize = 188
	// After ALIGN_TIMEOUT we stop insisting on motion and take any keyframe;
	// this is the extra grace before giving up on alignment entirely.
	relaxWindow = 2 * time.Second
	// A hold shorter than this is not a wait worth calling a slow path.
	happyHold = 400 * time.Millisecond
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
	waitAudio     bool
	renderTimeout time.Duration
	debug         bool
}

func logf(c *config, format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "streamgate[%s]: %s\n", c.tuner, fmt.Sprintf(format, a...))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return time.Duration(f * float64(time.Second))
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func loadConfig() (*config, error) {
	if len(os.Args) < 2 {
		return nil, errors.New("usage: streamgate <tuner-number>")
	}
	n := os.Args[1]
	c := &config{
		tuner:       n,
		tunerIP:     os.Getenv("TUNER" + n + "_IP"),
		encoderURL:  os.Getenv("ENCODER" + n + "_URL"),
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
		onTimeout:     env("ON_TIMEOUT", "fail"),
		alignKey:      env("ALIGN_KEYFRAME", "1") != "0",
		alignTimeout:  envDur("ALIGN_TIMEOUT", 8*time.Second),
		waitMotion:    env("WAIT_MOTION", "1") != "0",
		motionTimeout: envDur("MOTION_TIMEOUT", 6*time.Second),
		riseFactor:    envFloat("RISE_FACTOR", 5.0),
		motionHold:    envInt("MOTION_HOLD", 3),
		motionWindow:  envDur("MOTION_WINDOW", 250*time.Millisecond),
		waitAudio:     env("WAIT_AUDIO", "0") != "0",
		renderTimeout: envDur("RENDER_TIMEOUT", 3*time.Second),
		debug:         os.Getenv("DEBUG") != "",
	}
	if c.encoderURL == "" {
		return nil, fmt.Errorf("ENCODER%s_URL not set", n)
	}
	return c, nil
}

// ---------------------------------------------------------------- detection

var reID = regexp.MustCompile(`(?m)^\s*Id:\s*(\S+)`)

// secureCodecID returns the client id of an allocated secure video decoder, or
// "". Android hands out a new client per playback session, so a CHANGED id is
// the signal -- "a decoder exists" is not, since the previous channel's decoder
// may still be allocated when the gate starts.
func secureCodecID(dump string) string {
	lines := strings.Split(dump, "\n")
	inProc := false
	id := ""
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
				return id
			}
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

func adbShell(ctx context.Context, ip, cmd string) string {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "adb", "-s", ip, "shell", cmd).Output()
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(out), "\r", "")
}

// waitForVideo blocks until a new secure decoder has been seen `confirm` times.
func waitForVideo(ctx context.Context, c *config) error {
	exec.Command("adb", "connect", c.tunerIP).Run()

	// One round trip per poll: the resource dump, a marker, then the top
	// PlaybackState line.
	const probe = "dumpsys media.resource_manager; echo __MS__; dumpsys media_session | grep -m1 PlaybackState"
	split := func(out string) (string, string) {
		if i := strings.Index(out, "__MS__"); i >= 0 {
			return out[:i], out[i+len("__MS__"):]
		}
		return out, ""
	}

	rm, ms := split(adbShell(ctx, c.tunerIP, probe))
	base := secureCodecID(rm)
	baseSession := mediaSessionState(ms)
	// The session has no identity, so it only counts once it has dropped at
	// least once since we started -- otherwise a session left parked at
	// state=3 by the previous channel reads as instant success.
	armed := baseSession != "playing"
	if c.debug {
		logf(c, "baseline codec=%s session=%s armed=%v", orNone(base), baseSession, armed)
	}

	start := time.Now()
	hits := 0
	for {
		elapsed := time.Since(start)
		if elapsed >= c.tuneTimeout {
			return fmt.Errorf("no playback after %ds (base=%s)", int(elapsed.Seconds()), orNone(base))
		}

		rm, ms := split(adbShell(ctx, c.tunerIP, probe))
		id := secureCodecID(rm)
		session := mediaSessionState(ms)

		// A decoder that is not the one we started with is proof of new playback
		// and needs no arming, because it carries its own identity. The session
		// does not, so it only counts once armed.
		via := ""
		playing := false
		switch {
		case id != "" && id != base:
			playing, via = true, "codec "+id
		case session == "playing" && armed:
			playing, via = true, "session playing"
		}
		if session != "playing" {
			armed = true
		}

		if c.debug {
			logf(c, "t=%.1fs codec=%s base=%s session=%s armed=%v hits=%d playing=%v",
				elapsed.Seconds(), orNone(id), orNone(base), session, armed, hits, playing)
		}

		if playing && elapsed >= c.minWait {
			hits++
			if hits >= c.confirm {
				if c.settle > 0 {
					time.Sleep(c.settle)
				}
				logf(c, "playback detected after %ds via %s (base %s), %d confirmation(s)",
					int(time.Since(start).Seconds()), via, orNone(base), hits)
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
func audioStarted(ctx context.Context, c *config) bool {
	dump := adbShell(ctx, c.tunerIP, "dumpsys audio")
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
	for time.Since(start) < c.renderTimeout {
		if audioStarted(ctx, c) {
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
	for i+4 <= end && i+4 <= len(payload) {
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
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 1,
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("encoder returned %s", resp.Status)
	}

	br := bufio.NewReaderSize(resp.Body, 1<<20)

	// Write straight to stdout, unbuffered.
	//
	// A buffered writer here is a correctness bug, not an optimisation: it holds
	// video back until the buffer fills, so the DVR receives bursts instead of a
	// stream and buffers mid-playback. curl's -N exists for exactly this reason.
	// Reads are still buffered -- that side costs nothing.
	out := os.Stdout

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

	for {
		if !open {
			select {
			case <-gate:
				open = true
				gateAt = time.Now()
				if !c.alignKey {
					aligned = true
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
			logf(c, "no keyframe at all within %s; streaming unaligned", c.alignTimeout+relaxWindow)
		}

		if err := readPacket(br, pkt); err != nil {
			return err
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

		// Tell a loading screen from programming by how hard the picture is to
		// compress: a static card costs the encoder almost nothing, moving video
		// costs it everything. Nothing here is compared against a fixed bitrate,
		// because absolute numbers are meaningless across encoders, resolutions
		// and quality settings. The stream calibrates itself -- remember the
		// quietest window seen, and call it motion when a window rises well above
		// that floor and HOLDS, since the cut itself produces brief spikes.
		winBytes += tsPacketSize
		if now := time.Now(); now.Sub(winStart) >= c.motionWindow {
			rate := float64(winBytes) / now.Sub(winStart).Seconds()
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
			winBytes, winStart = 0, now
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
			logf(c, "aligned video-pid=%d discarded=%d packets/%.0fKB gate-to-air=%.2fs %s",
				p, discarded, float64(discarded*tsPacketSize)/1024,
				time.Since(gateAt).Seconds(), path)
			if lastPAT != nil {
				out.Write(lastPAT)
			}
			if lastPMT != nil {
				out.Write(lastPMT)
			}
		}

		if _, err := out.Write(pkt); err != nil {
			return err
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

	if err := <-streamErr; err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		logf(c, "stream ended: %v", err)
		os.Exit(1)
	}
}
