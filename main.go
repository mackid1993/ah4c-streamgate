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

const tsPacketSize = 188

type config struct {
	tuner       string
	tunerIP     string
	encoderURL  string
	minWait     time.Duration
	tuneTimeout time.Duration
	confirm     int
	poll        time.Duration
	confirmPoll time.Duration
	settle      time.Duration
	onTimeout   string
	alignKey    bool
	debug       bool
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
		settle:      envDur("SETTLE", 0),
		onTimeout:   env("ON_TIMEOUT", "fail"),
		alignKey:    env("ALIGN_KEYFRAME", "1") != "0",
		debug:       os.Getenv("DEBUG") != "",
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

	base := secureCodecID(adbShell(ctx, c.tunerIP, "dumpsys media.resource_manager"))
	if c.debug {
		logf(c, "baseline codec=%s", orNone(base))
	}

	start := time.Now()
	hits := 0
	for {
		elapsed := time.Since(start)
		if elapsed >= c.tuneTimeout {
			return fmt.Errorf("no playback after %ds (base=%s)", int(elapsed.Seconds()), orNone(base))
		}

		id := secureCodecID(adbShell(ctx, c.tunerIP, "dumpsys media.resource_manager"))
		playing := id != "" && id != base

		if c.debug {
			logf(c, "t=%.1fs codec=%s base=%s hits=%d playing=%v",
				elapsed.Seconds(), orNone(id), orNone(base), hits, playing)
		}

		if playing && elapsed >= c.minWait {
			hits++
			if hits >= c.confirm {
				if c.settle > 0 {
					time.Sleep(c.settle)
				}
				logf(c, "playback detected after %ds via codec %s (was %s), %d confirmation(s)",
					int(time.Since(start).Seconds()), id, orNone(base), hits)
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
	out := bufio.NewWriterSize(os.Stdout, 1<<18)
	defer out.Flush()

	var lastPAT, lastPMT []byte
	pmtPids := map[uint16]bool{}
	vidPids := map[uint16]bool{}

	open := false
	aligned := false
	pkt := make([]byte, tsPacketSize)
	discarded := 0

	for {
		if !open {
			select {
			case <-gate:
				open = true
				if !c.alignKey {
					aligned = true
				}
			default:
			}
		}

		if err := readPacket(br, pkt); err != nil {
			out.Flush()
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

		if !open {
			continue
		}

		if !aligned {
			// Only a video-PID random access point starts a decodable picture.
			if !(vidPids[p] && randomAccess(pkt)) {
				discarded++
				continue
			}
			aligned = true
			logf(c, "aligned to keyframe on pid %d after discarding %d packets (%.0f KB)",
				p, discarded, float64(discarded*tsPacketSize)/1024)
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
		close(gate)
	}

	if err := <-streamErr; err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		logf(c, "stream ended: %v", err)
		os.Exit(1)
	}
}
