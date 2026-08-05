package main

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
)

// curlVersion is the upstream release of the bundled static curl. Provenance
// and checksums live in third_party/README.md.
const curlVersion = "8.21.0"

// streamViaCurl hands delivery to the bundled curl, connected only NOW --
// after the gate -- with stdout inherited directly. ah4c treats CMDn's stdout
// as the tuner stream, so curl's output IS the stream, with this process out
// of the byte path entirely. Two things follow, and both are the point:
//
//  1. The encoder spent the whole detection wait with no client attached, so
//     its internal buffer is full. curl's first read delivers that backlog as
//     a burst, and the player then sits that far behind live for the whole
//     session -- the cushion that absorbs encoder and source hiccups. The
//     internal path structurally cannot provide this: it holds the connection
//     open through the wait, which consumes the buffer continuously, and its
//     gate discards what little remains.
//
//  2. The head of the stream is whatever the encoder buffered BEFORE the gate
//     opened -- the tail of the channel change. Keyframe alignment, table
//     injection and the motion gate do not run here; the DVR locks on
//     wherever it finds tables and a keyframe, exactly as under
//     streamgate.sh.
//
// READ_TIMEOUT maps onto curl's own low-speed abort so a wedged encoder still
// fails loudly instead of holding the tuner slot forever -- the one hang the
// shell-era pipeline had no answer to.
//
// The returned value is the process exit code: curl's success, and its exit
// 23 (a write error on stdout, i.e. the DVR hung up), are the normal ends of
// a recording.
func streamViaCurl(ctx context.Context, c *config) int {
	if len(curlBin) == 0 {
		logf(c, "DELIVERY=curl, but this build bundles no curl for %s/%s -- use a linux release binary or DELIVERY=internal", runtime.GOOS, runtime.GOARCH)
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

	secs := int(math.Ceil(c.readTimeout.Seconds()))
	cmd := exec.CommandContext(ctx, bin,
		"-sSN", "--fail",
		"--connect-timeout", "5",
		"--speed-limit", "1000", "--speed-time", strconv.Itoa(secs),
		c.encoderURL)
	// The *os.File itself, not a pipe: exec passes the descriptor straight to
	// curl, so delivery happens with this process out of the byte path. -sS
	// silences the progress meter but keeps curl's error text on stderr, next
	// to ours.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	logf(c, "handing the stream to bundled curl %s (READ_TIMEOUT=%v maps to: abort below 1000 B/s for %ds)",
		curlVersion, c.readTimeout, secs)
	err = cmd.Run()

	var ee *exec.ExitError
	switch {
	case err == nil:
		logf(c, "bundled curl exited cleanly -- the encoder ended the stream")
		return 0
	case errors.As(err, &ee) && ee.ExitCode() == 23:
		// curl 23 is a write error on its output. stdout is the DVR, so this
		// is the DVR hanging up: how a recording normally ends.
		logf(c, "stream closed by the DVR")
		return 0
	case errors.As(err, &ee) && ee.ExitCode() == 28:
		logf(c, "encoder fell below 1000 B/s for %ds (bundled curl exit 28) -- treating the stream as dead", secs)
		return 1
	case errors.As(err, &ee):
		logf(c, "bundled curl exited %d", ee.ExitCode())
		return 1
	default:
		logf(c, "bundled curl failed to run: %v", err)
		return 1
	}
}
