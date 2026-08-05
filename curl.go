package main

import (
	"bufio"
	"bytes"
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
	"sync"
	"syscall"
	"time"
)

// nullPacket is one MPEG-TS null packet (pid 0x1FFF): valid stream bytes that
// carry no picture. Used to keep the DVR's connection warm while the head is
// being shaped.
var nullPacket = func() []byte {
	p := make([]byte, tsPacketSize)
	p[0], p[1], p[2], p[3] = 0x47, 0x1f, 0xff, 0x10
	for i := 4; i < tsPacketSize; i++ {
		p[i] = 0xff
	}
	return p
}()

// curlVersion is the upstream release of the bundled static curl. Provenance
// and checksums live in third_party/README.md.
const curlVersion = "8.21.0"

// startMargin is how long past the gate output may not begin. Detection fires
// on decoder allocation, which runs 0.6-0.8s ahead of the first rendered
// pixel on this hardware, so the first moments after the gate can still be
// the tuning card; the margin also swallows the encoder's connect burst --
// its buffered cache of the channel change, delivered in the first instants
// -- and the encoder's own encode-to-wire lag. Output starts on the first
// keyframe past the margin: the live edge, as fast as a clean start can be.
//
// There is deliberately NO stall cushion in this design. Cushion seconds,
// seconds behind live, and extra tune seconds are the same number spent
// three ways, and the choice here is to spend none: a hiccup at the encoder
// longer than the player's own small buffer will surface as a stall, and the
// cure for those is encoder-side (CBR, a stable HDMI link, a box that is not
// wedged), not latency.
const startMargin = time.Second

// streamCurl is the delivery path: the bundled curl connects to the encoder
// the moment the gate opens -- never before, so detection costs the encoder
// nothing -- and its output is emitted with exactly one intervention, at the
// head. gated says detection actually confirmed playback; without it (no
// TUNERn_IP, or ON_TIMEOUT=stream) the output is a straight copy of whatever
// the encoder sends, which is what those modes have always meant.
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

	// The margin is anchored to the gate itself, not to the connect: a redial
	// on a later attempt must not pay it again.
	gateAt := time.Now()

	// While the head is being shaped -- margin, connect, keyframe hunt -- the
	// DVR is not left staring at a silent pipe: null packets keep the
	// connection warm so its probe overlaps these waits instead of following
	// them. Padding carries no picture and starts only on a gated tune, after
	// detection succeeded -- a failing tune still puts zero bytes on the wire.
	stopPad := func() {}
	if gated {
		var padWG sync.WaitGroup
		padStop := make(chan struct{})
		padWG.Add(1)
		go func() {
			defer padWG.Done()
			chunk := bytes.Repeat(nullPacket, 7)
			for {
				select {
				case <-padStop:
					return
				case <-time.After(50 * time.Millisecond):
					if _, werr := stdoutW.Write(chunk); werr != nil {
						return
					}
				}
			}
		}()
		var once sync.Once
		stopPad = func() {
			once.Do(func() {
				close(padStop)
				padWG.Wait()
			})
		}
		defer stopPad()
	}

	secs := int(math.Ceil(c.readTimeout.Seconds()))
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

		wrote, perr := deliver(c, pipe, gated, gateAt, stopPad)
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
		if attempt > maxPostGateDials || time.Since(gateAt) > startMargin+c.readTimeout {
			logf(c, "stream ended: %v", perr)
			return 1
		}
		logf(c, "encoder unavailable, no picture emitted yet (attempt %d: %v); redialling", attempt, perr)
		select {
		case <-ctx.Done():
			logf(c, "stream ended: %v", perr)
			return 1
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// deliver moves bytes from curl to stdout. On a gated tune the head is
// shaped: nothing but null padding is emitted inside the render margin, and
// output starts on the first keyframe past it with the newest tables in
// front. stopPad is called -- and waited on -- immediately before the first
// real write, so padding can never interleave with picture. Ungated tunes are
// a plain copy from the first byte.
func deliver(c *config, pipe io.Reader, gated bool, gateAt time.Time, stopPad func()) (wrote bool, err error) {
	if !gated {
		n, err := io.Copy(stdoutW, pipe)
		if err == nil {
			// io.Copy reports a clean end as nil; the caller needs "the encoder
			// ended the stream" to be distinguishable from success.
			err = io.EOF
		}
		return n > 0, err
	}

	br := bufio.NewReaderSize(pipe, 1<<16)
	pkt := make([]byte, tsPacketSize)
	relaxed := false
	warnedSync := false
	warn := func() {
		if !warnedSync {
			warnedSync = true
			logf(c, "no 188-byte sync grid in this stream; accepting bare sync bytes (is the encoder emitting MPEG-TS?)")
		}
	}

	// The newest tables seen while discarding are injected at the head, so
	// the DVR decodes from the very first packet instead of waiting for the
	// encoder's next table cycle. Same machinery the internal path proved out.
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

	marginAt := gateAt.Add(startMargin)
	huntUntil := marginAt.Add(c.alignTimeout + relaxWindow)
	discarded := 0
	aligned := false
	for {
		if rerr := readPacket(br, pkt, &relaxed, warn); rerr != nil {
			return false, fmt.Errorf("encoder sent no video (%w)", rerr)
		}
		scanPSI(pkt)
		now := time.Now()
		if now.Before(marginAt) {
			discarded++
			continue
		}
		if alignCandidate(pkt, pid(pkt), vidPids, pmtPids) {
			aligned = true
			break
		}
		discarded++
		if now.After(huntUntil) {
			break
		}
	}
	if !aligned {
		logf(c, "no keyframe recognised within %s of the margin; streaming unaligned (encoder may not signal random access)",
			c.alignTimeout+relaxWindow)
		if rerr := readPacket(br, pkt, &relaxed, warn); rerr != nil {
			return false, fmt.Errorf("encoder sent no video (%w)", rerr)
		}
	}
	logf(c, "aligned at the live edge %.2fs after the gate (%.1fs render margin + keyframe wait), discarded %.0fKB of pre-gate cache; stall cushion is zero by design",
		time.Since(gateAt).Seconds(), startMargin.Seconds(), float64(discarded*tsPacketSize)/1024)

	head := make([]byte, 0, len(lastPAT)+len(lastPMT)+tsPacketSize)
	head = append(head, lastPAT...)
	head = append(head, lastPMT...)
	head = append(head, pkt...)
	stopPad()
	wrote = true
	if _, werr := stdoutW.Write(head); werr != nil {
		return wrote, werr
	}

	// Out of the way: everything from here is a straight copy.
	_, err = io.Copy(stdoutW, br)
	if err == nil {
		err = io.EOF
	}
	return true, err
}
