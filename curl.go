package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// curlVersion is the upstream release of the bundled static curl. Provenance
// and checksums live in third_party/README.md.
const curlVersion = "8.21.0"

const (
	// stallGap is how long the encoder may go quiet before a null packet is
	// sent in its place. Short enough that a DVR never sees the bitstream
	// stop; long enough that it costs nothing on a healthy stream, where reads
	// arrive continuously and this timer never fires.
	stallGap = 500 * time.Millisecond
	// stallChunk is how much the pump reads at once. Small for the same reason
	// the emit batch is small: whatever sits in here is video taken off the
	// pipe but not yet emitted, so it is a ceiling on how far behind live we
	// can be.
	stallChunk = 1 << 13
)

// openStream is how streamOnce gets the encoder's bytes. A var so the streaming
// tests can supply an in-process HTTP source instead of exec'ing curl -- the
// same reason stdoutW is a var. Production is always curl.
var openStream = openViaCurl

// ---------------------------------------------------------------- bundled curl

var (
	curlOnce sync.Once
	curlPath string
	curlErr  error
)

// unpackCurl writes the embedded binary to a stable path and reuses it. Not a
// fresh temp dir per tune: a container tunes thousands of times over its life,
// and every one of those would leave a 10MB file behind for the OS to reap.
// The write goes to a unique name and is then renamed into place, so several
// tuners starting at once cannot serve each other a half-written binary.
func unpackCurl() (string, error) {
	curlOnce.Do(func() {
		if len(curlBin) == 0 {
			curlErr = fmt.Errorf("this build bundles no curl for %s/%s -- use a linux release binary", runtime.GOOS, runtime.GOARCH)
			return
		}
		final := filepath.Join(os.TempDir(), "streamgate-curl-"+curlVersion)
		if fi, err := os.Stat(final); err == nil && fi.Size() == int64(len(curlBin)) {
			curlPath = final
			return
		}
		tmp, err := os.CreateTemp(os.TempDir(), "streamgate-curl-*.part")
		if err != nil {
			curlErr = fmt.Errorf("cannot unpack bundled curl: %w", err)
			return
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(curlBin); err != nil {
			tmp.Close()
			curlErr = fmt.Errorf("cannot unpack bundled curl: %w", err)
			return
		}
		if err := tmp.Close(); err != nil {
			curlErr = fmt.Errorf("cannot unpack bundled curl: %w", err)
			return
		}
		if err := os.Chmod(tmp.Name(), 0o755); err != nil {
			curlErr = fmt.Errorf("cannot unpack bundled curl: %w", err)
			return
		}
		if err := os.Rename(tmp.Name(), final); err != nil {
			curlErr = fmt.Errorf("cannot unpack bundled curl: %w", err)
			return
		}
		curlPath = final
	})
	return curlPath, curlErr
}

// curlStream is the encoder connection: curl's stdout, plus the process behind
// it so Close can reap it.
type curlStream struct {
	io.ReadCloser
	cmd *exec.Cmd
	w   *os.File
}

func (s *curlStream) Close() error {
	// Kill first, then close. Closing our read end while curl still holds the
	// write end leaves it blocked on a write nobody will drain -- on a redial
	// that is one orphaned curl per attempt, each still holding a session on
	// an encoder that only grants a few.
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	err := s.ReadCloser.Close()
	_ = s.w.Close()
	_ = s.cmd.Wait()
	return err
}

// openViaCurl dials the encoder with the bundled curl and returns its stdout.
//
// curl does the HTTP, not this process: redirects, chunked transfer, odd
// keep-alive behaviour and the various ways a cheap encoder can be
// non-compliant are its problem, and it has spent thirty years learning them.
// What it does NOT get is the file descriptor -- stdout here is a pipe, so
// every byte still passes through streamgate. That is what keeps keyframe
// alignment, the pre-gate discard and null stall-filling possible; handing curl
// fd 1 directly would deliver the encoder's pre-gate buffer verbatim, splash
// screen and all, with nothing left in the process able to stop it.
//
// --fail so an HTTP error is an exit code rather than an error page recorded as
// video. -sS keeps curl's own diagnostics on stderr, next to ours. No
// --speed-limit: silence is the stall reader's business, and READ_TIMEOUT is
// enforced there, over the whole connection, rather than twice in two places
// that could disagree.
func openViaCurl(ctx context.Context, c *config) (io.ReadCloser, error) {
	bin, err := unpackCurl()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin,
		"-sSN", "--fail",
		"--connect-timeout", "5",
		c.encoderURL)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	// The child holds the only other reference to the write end. Without this
	// close, EOF never arrives on r when curl exits, and a dead encoder hangs
	// the read until READ_TIMEOUT instead of failing at once.
	_ = w.Close()
	return &curlStream{ReadCloser: r, cmd: cmd, w: w}, nil
}

// ------------------------------------------------------------- stall tolerance

// stallReader keeps the bitstream continuous while the encoder is not sending.
//
// A gap in a transport stream is not nothing: the DVR has to decide what an
// abrupt silence means, and what it decides is its own business and varies.
// Null packets remove the question. PID 0x1FFF is discarded by every demuxer,
// so the stream stays continuous without a byte of it reaching a decoder,
// appearing on screen, or disturbing the motion measurement and keyframe
// alignment downstream -- both of which already exclude it.
//
// What this is NOT: a diagnosis. It is not known that gaps are why recordings
// have stalled, and this must not be described as the cure for that. Its
// second job is to find out -- every gap it fills is logged, so a stutter with
// no such line rules the encoder going quiet out entirely.
//
// Silence is still bounded. Filling forever would turn a wedged encoder into a
// recording of nothing that never ends -- so once the quiet has run past
// READ_TIMEOUT the underlying failure is returned and the recording fails
// loudly, exactly as it did before.
type stallReader struct {
	ch      chan []byte
	errc    chan error
	c       *config
	gap     time.Duration
	budget  time.Duration
	src     io.ReadCloser
	pending []byte
	err     error
	quietAt time.Time
	filled  int
}

func newStallReader(c *config, src io.ReadCloser, gap, budget time.Duration) *stallReader {
	s := &stallReader{
		ch:     make(chan []byte),
		errc:   make(chan error, 1),
		c:      c,
		gap:    gap,
		budget: budget,
		src:    src,
	}
	// Unbuffered: the pump blocks until the consumer takes the chunk, so no
	// video is ever held here beyond one read. This layer must not become a
	// buffer -- it exists to stop gaps, not to introduce latency.
	go func() {
		for {
			buf := make([]byte, stallChunk)
			n, err := src.Read(buf)
			if n > 0 {
				s.ch <- buf[:n]
			}
			if err != nil {
				s.errc <- err
				return
			}
		}
	}()
	return s
}

func (s *stallReader) Read(b []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(b, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	t := time.NewTimer(s.gap)
	defer t.Stop()
	select {
	case chunk := <-s.ch:
		// One line per gap, on the way out of it, so the log says how long the
		// encoder was actually quiet rather than repeating itself twice a
		// second while it is. A recording that stutters with none of these did
		// not stutter because the encoder stopped sending.
		if s.filled > 0 {
			logf(s.c, "filled a %v gap in the encoder's output with %d null packet(s)",
				time.Since(s.quietAt).Round(10*time.Millisecond), s.filled)
			s.filled = 0
		}
		s.quietAt = time.Time{}
		n := copy(b, chunk)
		if n < len(chunk) {
			s.pending = chunk[n:]
		}
		return n, nil
	case err := <-s.errc:
		s.err = err
		return 0, err
	case <-t.C:
		if s.quietAt.IsZero() {
			s.quietAt = time.Now()
		}
		if s.budget > 0 && time.Since(s.quietAt) > s.budget {
			// Hand back a real failure rather than keep filling. Whoever is
			// waiting on this stream needs to hear that the encoder is gone.
			s.err = fmt.Errorf("encoder sent nothing for %v", s.budget)
			return 0, s.err
		}
		if len(b) < tsPacketSize {
			return 0, nil
		}
		s.filled++
		return copy(b, nullPacket()), nil
	}
}

func (s *stallReader) Close() error { return s.src.Close() }

// nullPacket is a stuffing TS packet: sync byte, PID 0x1FFF, payload only, and
// 184 bytes of stuffing.
func nullPacket() []byte {
	p := make([]byte, tsPacketSize)
	p[0], p[1], p[2], p[3] = 0x47, 0x1f, 0xff, 0x10
	for i := 4; i < tsPacketSize; i++ {
		p[i] = 0xff
	}
	return p
}
