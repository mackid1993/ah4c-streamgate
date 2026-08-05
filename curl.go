package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// curlVersion is the upstream release of the bundled static curl. Provenance
// and checksums live in third_party/README.md.
const curlVersion = "8.21.0"

const (
	// cushionBuild is how long the encoder is left unconnected after the gate,
	// filling its internal buffer with NEW-channel footage. Connecting then
	// delivers that footage as an instant burst, and the player runs that far
	// behind live for the whole session -- the cushion that absorbs hiccups.
	// The wait does not slow the tune the way it looks like it should: the DVR
	// probes the stream head before showing anything, and probing a burst
	// completes in milliseconds where probing a live trickle costs its full
	// length in real time.
	cushionBuild = 2500 * time.Millisecond
	// renderMargin is clipped off the oldest end of the kept footage. The gate
	// fires on decoder allocation, which runs 0.6-0.8s ahead of the first
	// rendered pixel on this hardware, so the first moments after the gate can
	// still be the tuning card. Keeping only footage younger than
	// cushionBuild-renderMargin makes the card structurally unable to reach
	// the output on any tune where rendering began within the margin.
	renderMargin = 750 * time.Millisecond
	// burstIdle separates the encoder's buffered burst from live delivery: a
	// packet that took this long to arrive came over the wire in real time,
	// not out of a buffer. Wider than the internal drain's 500us because the
	// bytes cross a child-process pipe with scheduler hops on both sides.
	burstIdle = 2 * time.Millisecond
	// The burst buffer is bounded by bytes and by wall clock so a pathological
	// encoder can neither exhaust memory nor stall the tune.
	maxBurstBytes = 32 << 20
	maxBurstWait  = 3 * time.Second
)

// pcrSeconds returns the packet's Program Clock Reference in seconds, if it
// carries one. The PCR is the encoder's own realtime clock, which is what
// lets the head trim measure footage age without trusting arrival timing.
func pcrSeconds(p []byte) (float64, bool) {
	afc := (p[3] >> 4) & 0x03
	if afc != 2 && afc != 3 {
		return 0, false
	}
	afLen := int(p[4])
	if afLen < 7 || p[5]&0x10 == 0 {
		return 0, false
	}
	base := uint64(p[6])<<25 | uint64(p[7])<<17 | uint64(p[8])<<9 |
		uint64(p[9])<<1 | uint64(p[10])>>7
	return float64(base) / 90000, true
}

// streamCurl is the delivery path: the bundled curl fetches the encoder --
// connected only after the gate, never before -- and its output is emitted
// with exactly one intervention, at the head. gated says detection actually
// confirmed playback; without it (no TUNERn_IP, or ON_TIMEOUT=stream) the
// output is a straight copy of whatever the encoder sends, which is what
// those modes have always meant.
//
// The head intervention is what reconciles the cushion with a clean start.
// The encoder's burst is buffered, and everything older than
// cushionBuild-renderMargin -- measured on the stream's own PCR clock -- is
// discarded: by construction that discard covers every byte from before the
// gate plus the render margin, so the channel-change tail cannot reach the
// output. What remains is new-channel footage, emitted from its first
// keyframe with the newest tables in front so the DVR locks instantly, and
// still ahead of real time: the player starts with the cushion in hand.
// After the head, bytes pass through untouched.
//
// READ_TIMEOUT maps onto curl's own low-speed abort so a wedged encoder
// still fails loudly instead of holding the tuner slot forever.
func streamCurl(ctx context.Context, c *config, gated bool) int {
	if len(curlBin) == 0 {
		logf(c, "this build bundles no curl for %s/%s -- only the linux release binaries can stream", runtime.GOOS, runtime.GOARCH)
		return 1
	}
	dir, err := os.MkdirTemp("", "streamgate-curl-")
	if err != nil {
		logf(c, "cannot unpack bundled curl: %v", err)
		return 1
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "curl")
	if err := os.WriteFile(bin, curlBin, 0o755); err != nil {
		logf(c, "cannot unpack bundled curl: %v", err)
		return 1
	}

	if gated {
		// The whole point of not connecting yet: let the encoder buffer clean
		// footage for the burst. See cushionBuild.
		time.Sleep(cushionBuild)
	}

	secs := int(math.Ceil(c.readTimeout.Seconds()))
	start := time.Now()
	for attempt := 1; ; attempt++ {
		cmd := exec.CommandContext(ctx, bin,
			"-sSN", "--fail",
			"--connect-timeout", "5",
			"--speed-limit", "1000", "--speed-time", strconv.Itoa(secs),
			c.encoderURL)
		cmd.Stderr = os.Stderr
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			logf(c, "cannot pipe bundled curl: %v", err)
			return 1
		}
		if err := cmd.Start(); err != nil {
			logf(c, "bundled curl failed to run: %v", err)
			return 1
		}
		if attempt == 1 {
			logf(c, "fetching via bundled curl %s (READ_TIMEOUT=%v maps to: abort below 1000 B/s for %ds)",
				curlVersion, c.readTimeout, secs)
		}

		wrote, perr := deliver(c, pipe, gated)
		// deliver can return while curl still runs -- a write failure is the
		// DVR hanging up, not curl ending. Kill is a no-op on a curl that has
		// already exited (its status is preserved for Wait), so this reaps
		// safely either way.
		_ = cmd.Process.Kill()
		werr := cmd.Wait()

		// When curl's low-speed abort ended the stream, deliver only saw EOF;
		// "the encoder went silent" is the truth the log must carry.
		var ee *exec.ExitError
		if errors.As(werr, &ee) && ee.ExitCode() == 28 {
			logf(c, "encoder fell below 1000 B/s for %ds (bundled curl exit 28) -- treating the stream as dead", secs)
		}

		switch {
		case errors.Is(perr, syscall.EPIPE), errors.Is(perr, os.ErrClosed):
			// The DVR hung up. That is how a recording normally ends.
			logf(c, "stream closed by the DVR")
			return 0
		case wrote, ctx.Err() != nil:
			if errors.Is(perr, io.EOF) {
				logf(c, "encoder ended the stream")
			} else {
				logf(c, "stream ended: %v", perr)
			}
			return 1
		}
		// Nothing emitted: the encoder refused or died at connect. The gate is
		// open and the DVR is waiting, so retries are bounded.
		if attempt > maxPostGateDials || time.Since(start) > c.readTimeout {
			logf(c, "stream ended: %v", perr)
			return 1
		}
		logf(c, "encoder unavailable, nothing emitted yet (attempt %d: %v); redialling", attempt, perr)
		select {
		case <-ctx.Done():
			logf(c, "stream ended: %v", perr)
			return 1
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// deliver moves bytes from curl to stdout. Trimming runs only on a gated
// tune; otherwise this is a plain copy from the first byte.
func deliver(c *config, pipe io.Reader, trim bool) (wrote bool, err error) {
	if !trim {
		n, err := io.Copy(stdoutW, pipe)
		if err == nil {
			// io.Copy reports a clean end as nil; the caller needs "the encoder
			// ended the stream" to be distinguishable from success.
			err = io.EOF
		}
		return n > 0, err
	}

	// Phase 1: take the encoder's buffered burst into memory. Arrival timing
	// tells buffered from live apart, exactly like the internal drain did: a
	// buffered packet is handed over in microseconds, a live one has to wait
	// for the encoder's real-time clock.
	br := bufio.NewReaderSize(pipe, 1<<16)
	pkt := make([]byte, tsPacketSize)
	backlog := make([]byte, 0, 1<<20)
	relaxed := false
	warnedSync := false
	warn := func() {
		if !warnedSync {
			warnedSync = true
			logf(c, "no 188-byte sync grid in this stream; accepting bare sync bytes (is the encoder emitting MPEG-TS?)")
		}
	}
	burstStart := time.Now()
	var readErr error
	for len(backlog)+tsPacketSize <= maxBurstBytes && time.Since(burstStart) < maxBurstWait {
		t0 := time.Now()
		if err := readPacket(br, pkt, &relaxed, warn); err != nil {
			readErr = err
			break
		}
		backlog = append(backlog, pkt...)
		if time.Since(t0) > burstIdle && len(backlog) > tsPacketSize {
			// This packet waited on the wire: the buffer is drained and we are
			// at the live edge.
			break
		}
	}
	if len(backlog) == 0 {
		if readErr == nil {
			readErr = errors.New("no data in the connect burst")
		}
		return false, fmt.Errorf("encoder sent no video (%w)", readErr)
	}

	// Phase 2: find the cut. Everything older than the kept window is from
	// before the gate (plus the render margin) and is discarded unseen.
	keep := (cushionBuild - renderMargin).Seconds()
	var firstPCR, lastPCR float64
	havePCR := false
	for i := 0; i+tsPacketSize <= len(backlog); i += tsPacketSize {
		if v, ok := pcrSeconds(backlog[i : i+tsPacketSize]); ok {
			if !havePCR {
				firstPCR, havePCR = v, true
			}
			lastPCR = v
		}
	}
	cut := len(backlog)
	switch {
	case !havePCR || lastPCR < firstPCR:
		// No usable clock (or it wrapped mid-burst). The safe direction is a
		// clean start with no cushion, never a cushion that might carry the
		// card: discard the whole burst and continue from live.
		logf(c, "no usable PCR clock in the connect burst; starting from live without a cushion")
	case lastPCR-firstPCR <= keep:
		// The encoder buffers less than the window we would keep, so the whole
		// burst is younger than the cut. Keep all of it.
		cut = 0
	default:
		target := lastPCR - keep
		for i := 0; i+tsPacketSize <= len(backlog); i += tsPacketSize {
			if v, ok := pcrSeconds(backlog[i : i+tsPacketSize]); ok && v >= target {
				cut = i
				break
			}
		}
	}

	// Phase 3: cache the newest tables seen up to the cut, then start on the
	// first keyframe at or after it. The table and keyframe machinery is the
	// same code the internal path proved out.
	var lastPAT, lastPMT []byte
	pmtPids := map[uint16]bool{}
	vidPids := map[uint16]bool{}
	scanPSI := func(p []byte) {
		id := pid(p)
		if id == 0 {
			if pl := psiPayload(p, 0x00); pl != nil && sectionComplete(pl) {
				lastPAT = append(lastPAT[:0], p...)
				if m := pmtPIDs(pl); len(m) > 0 {
					if !sameSet(m, pmtPids) {
						lastPMT = lastPMT[:0]
						vidPids = map[uint16]bool{}
					}
					pmtPids = m
				}
			}
		} else if pmtPids[id] {
			if pl := psiPayload(p, 0x02); pl != nil && sectionComplete(pl) {
				if v := videoPIDs(pl); len(v) > 0 {
					vidPids = v
					lastPMT = append(lastPMT[:0], p...)
				}
			}
		}
	}
	kf := -1
	for i := 0; i+tsPacketSize <= len(backlog); i += tsPacketSize {
		p := backlog[i : i+tsPacketSize]
		scanPSI(p)
		if kf < 0 && i >= cut && alignCandidate(p, pid(p), vidPids, pmtPids) {
			kf = i
		}
	}

	emit := func(head []byte) error {
		out := make([]byte, 0, len(lastPAT)+len(lastPMT)+len(head))
		out = append(out, lastPAT...)
		out = append(out, lastPMT...)
		out = append(out, head...)
		if len(out) > 0 {
			wrote = true
			if _, werr := stdoutW.Write(out); werr != nil {
				return werr
			}
		}
		return nil
	}

	if kf >= 0 {
		cushion := 0.0
		if havePCR {
			if v, ok := firstPCRAt(backlog, kf); ok {
				cushion = lastPCR - v
			}
		}
		logf(c, "aligned in the connect burst: kept %.0fKB (~%.1fs cushion), discarded %.0fKB of pre-gate footage",
			float64(len(backlog)-kf)/1024, cushion, float64(kf)/1024)
		if werr := emit(backlog[kf:]); werr != nil {
			return wrote, werr
		}
	} else {
		// No keyframe in the kept window: wait for the next one live, bounded,
		// rather than ship a head the DVR cannot decode. The cushion is lost
		// but the start stays clean.
		logf(c, "no keyframe in the kept footage; aligning on the live stream")
		deadline := time.Now().Add(c.alignTimeout + relaxWindow)
		aligned := false
		for time.Now().Before(deadline) {
			if err := readPacket(br, pkt, &relaxed, warn); err != nil {
				if !wrote {
					return false, fmt.Errorf("encoder sent no video (%w)", err)
				}
				return wrote, err
			}
			scanPSI(pkt)
			if alignCandidate(pkt, pid(pkt), vidPids, pmtPids) {
				aligned = true
				break
			}
		}
		if !aligned {
			logf(c, "no keyframe recognised within %s; streaming unaligned", c.alignTimeout+relaxWindow)
			if err := readPacket(br, pkt, &relaxed, warn); err != nil {
				return wrote, err
			}
		}
		if werr := emit(pkt); werr != nil {
			return wrote, werr
		}
	}
	if readErr != nil {
		return wrote, readErr
	}

	// Phase 4: out of the way. Everything from here is a straight copy.
	_, err = io.Copy(stdoutW, br)
	if err == nil {
		err = io.EOF
	}
	return true, err
}

// firstPCRAt returns the first PCR at or after byte offset i.
func firstPCRAt(b []byte, i int) (float64, bool) {
	for ; i+tsPacketSize <= len(b); i += tsPacketSize {
		if v, ok := pcrSeconds(b[i : i+tsPacketSize]); ok {
			return v, true
		}
	}
	return 0, false
}
