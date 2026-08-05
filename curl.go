package main

import (
	"context"
	"errors"
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
	// gapReport is how long the encoder may go quiet before the silence is
	// worth a log line. At any real bitrate a read lands every few tens of
	// milliseconds, so silence this long is unambiguous.
	gapReport = 150 * time.Millisecond
	// readChunk is how much the pump reads at once. Small for the same reason
	// the emit batch is small: whatever sits in here is video taken off the
	// pipe but not yet emitted, so it is a ceiling on how far behind live we
	// can be.
	readChunk = 1 << 13
)

// openStream is how streamOnce gets the encoder's bytes. A var so the streaming
// tests can supply an in-process HTTP source instead of exec'ing the bundled
// curl -- the same reason stdoutW is a var. Production is always curl.
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
// it so its exit status can be read and Close can reap it.
type curlStream struct {
	io.ReadCloser
	cmd  *exec.Cmd
	w    *os.File
	once sync.Once
	werr error
}

// Read turns curl's exit status into the error the stream ends with.
//
// Without this the pipe simply reaches EOF and every curl failure -- a 503, a
// refused connection, a DNS miss -- was reported as "encoder sent no video
// (EOF)". That names the wrong fault: the encoder answered, it just answered
// with an error. curl's own message is already on stderr directly above, so
// this adds the exit code and leaves the diagnosis to it.
func (s *curlStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err == io.EOF {
		if werr := s.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// encoderFault is an error that already names what the encoder did. Callers
// must return it as-is rather than wrapping it in a diagnosis of their own --
// "encoder sent no video" on top of a 503 names a silence that never happened.
type encoderFault struct{ s string }

func (e encoderFault) Error() string { return e.s }

func (s *curlStream) wait() error {
	s.once.Do(func() { s.werr = s.cmd.Wait() })
	var ee *exec.ExitError
	if !errors.As(s.werr, &ee) {
		return nil
	}
	switch code := ee.ExitCode(); code {
	case 22:
		// --fail turns any HTTP error status into this. curl printed the
		// status itself on the line above.
		return encoderFault{"encoder refused the request (bundled curl exit 22)"}
	case 7:
		return encoderFault{"cannot reach the encoder (bundled curl exit 7)"}
	case 28:
		return encoderFault{"encoder timed out (bundled curl exit 28)"}
	default:
		return encoderFault{fmt.Sprintf("bundled curl exited %d", code)}
	}
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
	s.once.Do(func() { s.werr = s.cmd.Wait() })
	return err
}

// openViaCurl dials the encoder with the bundled curl and returns its stdout.
//
// curl does the HTTP, not this process: redirects, chunked transfer, odd
// keep-alive behaviour and the various ways a cheap encoder can be
// non-compliant are its problem, and it has spent thirty years learning them.
// What it does NOT get is the file descriptor -- stdout here is a pipe, so
// every byte still passes through streamgate. That is what keeps keyframe
// alignment and the pre-gate discard possible; handing curl fd 1 directly would
// deliver the encoder's pre-gate buffer verbatim, splash screen and all, with
// nothing left in the process able to stop it.
//
// --fail so an HTTP error is an exit code rather than an error page recorded as
// video. -sS keeps curl's own diagnostics on stderr, next to ours. No
// --speed-limit: silence is idleReader's business, and READ_TIMEOUT is enforced
// there rather than twice in two places that could disagree.
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

// ------------------------------------------------------------------ read side

// idleReader fails a read that goes quiet for too long, instead of blocking on
// it forever. It is what enforces READ_TIMEOUT over curl's pipe, where the
// socket deadline the in-process path uses is not available.
//
// It also times the encoder's silences and logs any that pass gapReport. That
// is a measurement, not a remedy, and the distinction is the point: it writes
// NOTHING. An earlier version filled these gaps with null packets, which was
// wrong twice over -- the injected packets went through readPacket like real
// ones, so synthetic data drove streamgate's own state machine and the
// ALIGN_TIMEOUT fallback fired during a silence instead of waiting for the
// encoder; and more fundamentally a null packet carries no frame, so filling a
// gap cannot stop a player stuttering through it. It only stops a receiver
// concluding the stream ended. Nothing here has ever needed that.
//
// What the log line is for: if recordings stutter and no gap is ever reported,
// the encoder going quiet is ruled out and the cause lies elsewhere.
type idleReader struct {
	c        *config
	ch       chan []byte
	errc     chan error
	budget   time.Duration
	src      io.ReadCloser
	pending  []byte
	err      error
	lastData time.Time
}

func newIdleReader(c *config, src io.ReadCloser, budget time.Duration) *idleReader {
	s := &idleReader{
		c:        c,
		ch:       make(chan []byte),
		errc:     make(chan error, 1),
		budget:   budget,
		src:      src,
		lastData: time.Now(),
	}
	// Unbuffered: the pump blocks until the consumer takes the chunk, so no
	// video is ever held here beyond one read. This layer must not become a
	// buffer -- it exists to bound silence, not to introduce latency.
	go func() {
		for {
			buf := make([]byte, readChunk)
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

func (s *idleReader) Read(b []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(b, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	if s.budget <= 0 {
		select {
		case chunk := <-s.ch:
			return s.take(b, chunk), nil
		case err := <-s.errc:
			s.err = err
			return 0, err
		}
	}
	t := time.NewTimer(s.budget)
	defer t.Stop()
	select {
	case chunk := <-s.ch:
		return s.take(b, chunk), nil
	case err := <-s.errc:
		s.err = err
		return 0, err
	case <-t.C:
		s.err = fmt.Errorf("encoder sent nothing for %v", s.budget)
		return 0, s.err
	}
}

func (s *idleReader) take(b, chunk []byte) int {
	// Report the silence that just ended. Measured on arrival, because that is
	// the only moment its length is known.
	if d := time.Since(s.lastData); d >= gapReport {
		logf(s.c, "gap in the encoder's output: %v", d.Round(10*time.Millisecond))
	}
	s.lastData = time.Now()
	n := copy(b, chunk)
	if n < len(chunk) {
		s.pending = chunk[n:]
	}
	return n
}

func (s *idleReader) Close() error { return s.src.Close() }
