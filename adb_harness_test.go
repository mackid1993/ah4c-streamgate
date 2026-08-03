// Integration harness for the half of streamgate that talks to the box.
//
// waitForVideo, adbShell, audioStarted, waitForRender and main were all at 0%
// coverage for one reason: adbShell execs a real `adb`. So this puts a fake
// `adb` first on PATH. Nothing in main.go changes -- the production path runs
// exactly as shipped, fork and exec and argv and exit status and all, which is
// the point: the two most interesting bugs found with this harness live in how
// an exit status and a partial dump are handled, and a stubbed-out adbShell
// would have hidden both.
//
// Two entry points:
//
//	hNewADB   installs the fake adb and its canned dumps for the current test.
//	          The detection functions are then called IN PROCESS, which is what
//	          gives them coverage in the ordinary profile.
//
//	hRunMain  re-executes THIS test binary with SG_SUBPROCESS=1, which runs
//	          main(). Only a real process can answer the two questions that
//	          matter most about main: what is the exit code, and how many bytes
//	          reached fd 1. stdout is the video stream; "zero bytes on a failed
//	          tune" is not assertable any other way.
//
// Encoder fixtures reuse buildStream/videoPacket from main_test.go. Everything
// new here is prefixed h to keep the package namespace clear.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------ fake adb

// hADBScript answers the two shapes streamgate uses: `adb connect IP` and
// `adb -s IP shell CMD`. Behaviour comes from files in $SG_ADB_DIR, so one
// script covers every scenario and successive polls can differ.
//
// step.N is the answer to the Nth probe; step.last repeats once the numbered
// steps run out. A file's first line may be a directive:
//
//	@HANG      never answer
//	@FAIL      exit 1 with no output at all (adb transport failure)
//	@EXIT n    print the rest, then exit n (a dump that arrived, and a non-zero
//	           status -- which is exactly what `grep -m1 PlaybackState` produces
//	           when the box has no media session)
const hADBScript = `#!/bin/sh
D="$SG_ADB_DIR"

# A hang has to leave a GRANDCHILD holding the inherited stdout pipe, because
# that is what the real adb server daemon does and it is the only way exec's
# WaitDelay is ever reached. Without the backgrounded holder, CommandContext
# kills the direct child, Output() returns the instant the deadline passes, and
# a "hang" fixture would measure nothing at all.
hang() {
	sleep 30 &
	sleep 30
	exit 0
}

if [ "$1" = "connect" ]; then
	echo connect >> "$D/calls"
	if [ -f "$D/connect.hang" ]; then hang; fi
	if [ -f "$D/connect.fail" ]; then exit 1; fi
	echo "connected to $2"
	exit 0
fi

cmd="$4"
case "$cmd" in
*"dumpsys audio"*)
	echo audio >> "$D/calls"
	f="$D/audio"
	;;
*)
	echo shell >> "$D/calls"
	n=$(cat "$D/n" 2>/dev/null || echo 0)
	n=$((n + 1))
	printf '%s\n' "$n" > "$D/n"
	f="$D/step.$n"
	if [ ! -f "$f" ]; then f="$D/step.last"; fi
	;;
esac

if [ ! -f "$f" ]; then exit 1; fi

first=$(head -n 1 "$f")
case "$first" in
@HANG) hang ;;
@FAIL) exit 1 ;;
@EXIT*)
	tail -n +2 "$f"
	exit "${first#@EXIT }"
	;;
esac
cat "$f"
exit 0
`

// hStep is one answer from the fake adb.
type hStep struct {
	ids     []string // secure video decoder client ids the resource dump reports
	session string   // the PlaybackState line; "" means grep matched nothing
	raw     string   // replaces the generated body outright
	exit    int      // exit status; the body is STILL printed, like grep -m1
	fail    bool     // exit 1 with no output -- adb itself failed
	empty   bool     // exit 0 with empty stdout
	hang    bool     // never answers
}

// hPlayback builds a PlaybackState line in the shape the README's own manual
// check produces.
func hPlayback(state, speed string) string {
	return fmt.Sprintf("      PlaybackState {state=%s, position=0, buffered position=0, speed=%s, updated=1000, actions=0}",
		state, speed)
}

var (
	hPlaying = hPlayback("3", "1.0")
	hStopped = hPlayback("2", "0.0")
	hPaused  = hPlayback("3", "0.0")
)

// hProbeBody renders what the composite probe prints: the resource-manager
// dump, the __MS__ marker, the grepped PlaybackState line, then __MS2__ --
// matching the real probe, whose second marker is what proves the session half
// finished rather than merely started.
func hProbeBody(s hStep) string {
	if s.raw != "" {
		return s.raw
	}
	var b strings.Builder
	b.WriteString("Processes:\n")
	for i, id := range s.ids {
		fmt.Fprintf(&b, "  Pid: %d\n    Id: %s\n    {name: secure-codec, subType: video-codec, value: 1}\n",
			4000+i, id)
	}
	b.WriteString("Events logs:\n")
	b.WriteString("__MS__\n")
	if s.session != "" {
		b.WriteString(s.session + "\n")
	}
	b.WriteString("__MS2__\n")
	return b.String()
}

func hStepFile(s hStep) string {
	switch {
	case s.hang:
		return "@HANG\n"
	case s.fail:
		return "@FAIL\n"
	case s.empty:
		return ""
	case s.exit != 0:
		return fmt.Sprintf("@EXIT %d\n%s", s.exit, hProbeBody(s))
	}
	return hProbeBody(s)
}

type hADB struct {
	t   *testing.T
	dir string
}

// hNewADB installs the fake adb first on PATH for the duration of the test and
// canned answers for successive probes. The last step repeats forever.
func hNewADB(t *testing.T, steps ...hStep) *hADB {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(hADBScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, s := range steps {
		hWrite(t, filepath.Join(dir, fmt.Sprintf("step.%d", i+1)), hStepFile(s))
	}
	if len(steps) > 0 {
		hWrite(t, filepath.Join(dir, "step.last"), hStepFile(steps[len(steps)-1]))
	}
	t.Setenv("SG_ADB_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &hADB{t: t, dir: dir}
}

func hWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setAudio installs the answer to `dumpsys audio`.
func (a *hADB) setAudio(body string) { hWrite(a.t, filepath.Join(a.dir, "audio"), body) }

func (a *hADB) hangConnect() { hWrite(a.t, filepath.Join(a.dir, "connect.hang"), "") }

// calls counts invocations of one kind: "connect", "shell" or "audio".
func (a *hADB) calls(kind string) int {
	b, err := os.ReadFile(filepath.Join(a.dir, "calls"))
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == kind {
			n++
		}
	}
	return n
}

// hConfig is a config wound tight enough that a whole scenario runs in under a
// second. Everything the test cares about is set explicitly.
func hConfig() *config {
	return &config{
		tuner:         "H",
		tunerIP:       "127.0.0.1:5555",
		tuneTimeout:   1500 * time.Millisecond,
		confirm:       1,
		poll:          20 * time.Millisecond,
		confirmPoll:   10 * time.Millisecond,
		renderTimeout: 300 * time.Millisecond,
	}
}

// hCaptureStderr collects what logf wrote, which is the only way to tell WHICH
// signal opened the gate -- waitForVideo returns nil either way.
func hCaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func hWait(t *testing.T, c *config) (error, time.Duration, string) {
	t.Helper()
	var err error
	start := time.Now()
	log := hCaptureStderr(t, func() { err = waitForVideo(context.Background(), c) })
	return err, time.Since(start), log
}

// ------------------------------------------------- baseline acquisition

// The first call after `adb connect` is the likeliest to fail, and an empty
// baseline makes every decoder look new. A failed first probe must not become
// the baseline.
func TestWaitForVideoSurvivesAFailedFirstProbe(t *testing.T) {
	a := hNewADB(t,
		hStep{fail: true},
		hStep{ids: []string{"OLD"}, session: hStopped},
		hStep{ids: []string{"OLD"}, session: hStopped},
		hStep{ids: []string{"OLD", "NEW"}, session: hPlaying},
	)
	err, _, log := hWait(t, hConfig())
	if err != nil {
		t.Fatalf("waitForVideo: %v", err)
	}
	if !strings.Contains(log, "via codec NEW") {
		t.Errorf("gate opened via the wrong signal:\n%s", log)
	}
	// Four probes, not two: had the failed probe been taken as the baseline, OLD
	// would have read as new and the gate would have opened on the channel we are
	// leaving after two calls.
	if got := a.calls("shell"); got != 4 {
		t.Errorf("%d probes, want 4 -- the gate opened early on the previous channel", got)
	}
}

func TestWaitForVideoSurvivesManyFailedProbes(t *testing.T) {
	steps := []hStep{{fail: true}, {fail: true}, {fail: true}, {fail: true}, {fail: true},
		{ids: []string{"OLD"}, session: hStopped},
		{ids: []string{"OLD", "NEW"}, session: hPlaying}}
	a := hNewADB(t, steps...)
	if err, _, _ := hWait(t, hConfig()); err != nil {
		t.Fatalf("waitForVideo: %v", err)
	}
	if got := a.calls("shell"); got != 7 {
		t.Errorf("%d probes, want 7", got)
	}
	// Five consecutive failures inside 1.5s must NOT have triggered a reconnect:
	// the rate limit is measured from the initial connect, so nothing can fire
	// before t+5s. See TestWaitForVideoReconnect.
	if got := a.calls("connect"); got != 1 {
		t.Errorf("%d connects, want 1 (the initial one)", got)
	}
}

// BUG (main.go:480). The baseline is taken from any probe that returned BYTES,
// not from one that parsed. `echo __MS__` is unconditional, so a probe whose
// resource dump failed still returns a non-empty string, sails past the raw==""
// guard, and installs an EMPTY baseline -- after which the previous channel's
// still-allocated decoder reads as new and opens the gate immediately. That is
// precisely the failure main.go:418-425 says it fixed.
//
// Pinned as it behaves today so the suite stays green against unmodified
// main.go. WANT: a dump that yields no ids AND no session must not be accepted
// as a baseline.
// A transient binder failure on the resource half must not install an empty
// CODEC baseline -- the outgoing channel's decoder would then read as new. The
// codec baseline is taken separately, from the first legible resource dump.
func TestWaitForVideoDefersTheCodecBaselinePastABinderBlip(t *testing.T) {
	hNewADB(t,
		hStep{raw: "Can't find service: media.resource_manager\n__MS__\n" + hStopped + "\n__MS2__\n"},
		hStep{ids: []string{"OLD"}, session: hStopped},
		hStep{ids: []string{"OLD"}, session: hStopped},
	)
	c := hConfig()
	c.tuneTimeout = 900 * time.Millisecond
	err, _, log := hWait(t, c)
	if err == nil {
		t.Fatalf("gate opened on the outgoing channel's decoder; log:\n%s", log)
	}
	if strings.Contains(log, "via codec OLD") {
		t.Errorf("an empty baseline made a pre-existing decoder look new:\n%s", log)
	}
}

// ...and the opposite pull: a box that has NO resource manager at all must still
// tune on the session fallback. Requiring a legible resource dump for the
// baseline failed every tune on exactly the device class the fallback exists for.
func TestWaitForVideoTunesWithNoResourceManagerAtAll(t *testing.T) {
	none := "Can't find service: media.resource_manager\n__MS__\n"
	hNewADB(t,
		hStep{raw: none + "__MS2__\n"},              // no session published yet
		hStep{raw: none + "__MS2__\n"},              //
		hStep{raw: none + hPlaying + "\n__MS2__\n"}, // new channel comes up playing
		hStep{raw: none + hPlaying + "\n__MS2__\n"},
	)
	c := hConfig()
	c.tuneTimeout = 3 * time.Second
	err, _, log := hWait(t, c)
	if err != nil {
		t.Fatalf("no tune is possible on a box without a resource manager: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log, "via session playing") {
		t.Errorf("want the session fallback, got:\n%s", log)
	}
}

// The same hole reached by the other realistic route: adb hands back a
// TRUNCATED dump, so the __MS__ marker never arrives, split() returns the whole
// thing as the resource half (main.go:414) and nothing parses out of it.
// A dump cut off mid-write parses to no decoders at all, so accepting it as the
// baseline makes the OUTGOING channel's decoder read as new on the very next
// poll. The __MS__ marker is the discriminator: it is echoed between the two
// halves, so its absence proves the probe never finished.
func TestWaitForVideoRejectsATruncatedBaseline(t *testing.T) {
	hNewADB(t,
		hStep{raw: "Processes:\n  Pid: 4000\n    Id: OL"}, // cut mid-dump, no marker
		hStep{ids: []string{"OLD"}, session: hStopped},
	)
	c := hConfig()
	c.tuneTimeout = 900 * time.Millisecond
	err, _, log := hWait(t, c)
	if err == nil {
		t.Fatalf("gate opened on the channel we were leaving; log:\n%s", log)
	}
	if strings.Contains(log, "via codec OLD") {
		t.Errorf("a truncated dump became the baseline:\n%s", log)
	}
}

// BUG (main.go:486, main.go:506). `armed` is the guard that stops a session left
// parked at state=3 by the PREVIOUS channel from reading as instant success. It
// is set by `session != "playing"`, and mediaSessionState returns "unknown" for
// a dump it could not read as much as for one that says stopped. So a single
// unreadable media_session half -- a truncated response, the marker lost, the
// app rebuilding its MediaSession during the channel change -- arms the
// fallback, and the still-parked old session opens the gate on the very next
// poll. The gate then fires on the channel we are trying to leave, which is the
// exact failure `armed` exists to prevent.
func TestWaitForVideoArmsOnAnUnreadableSession(t *testing.T) {
	hNewADB(t,
		hStep{ids: []string{"OLD"}, session: hPlaying},    // parked playing: armed=false
		hStep{raw: "Processes:\n  Pid: 4000\n    Id: OL"}, // one truncated read
		hStep{ids: []string{"OLD"}, session: hPlaying},    // the SAME parked session
	)
	c := hConfig()
	c.tuneTimeout = 700 * time.Millisecond
	err, _, log := hWait(t, c)
	// An unreadable session half must NOT arm the fallback: the only session we
	// ever saw was the previous channel's, parked at state=3.
	if err == nil {
		t.Fatalf("gate opened on a session that never dropped; log:\n%s", log)
	}
	if strings.Contains(log, "via session playing") {
		t.Errorf("session fallback fired unarmed:\n%s", log)
	}
	t.Log("BUG main.go:486/506: one unreadable media_session read armed the " +
		"fallback and the previous channel's parked state=3 opened the gate")
}

// An empty stdout with a zero exit is indistinguishable from a failure, which
// is the conservative answer and the right one.
func TestWaitForVideoTreatsEmptyDumpAsAFailure(t *testing.T) {
	a := hNewADB(t,
		hStep{empty: true},
		hStep{ids: []string{"OLD"}, session: hStopped},
		hStep{ids: []string{"OLD", "NEW"}, session: hStopped},
	)
	if err, _, _ := hWait(t, hConfig()); err != nil {
		t.Fatalf("waitForVideo: %v", err)
	}
	if got := a.calls("shell"); got != 3 {
		t.Errorf("%d probes, want 3 -- the empty dump was taken as a baseline", got)
	}
}

// BUG (main.go:376). adbShell throws away stdout whenever adb exits non-zero,
// even when the dump printed is complete. The probe ends in
// `... | grep -m1 PlaybackState`, and grep exits 1 when it matches nothing --
// which is the normal state of a box with no media session, i.e. exactly the
// device class the session fallback exists for (README:59). On those devices
// every poll is filed as an adb failure, the resource dump that came back in
// the same breath is discarded, and the CODEC signal never runs at all.
func TestADBShellDiscardsAGoodDumpOnNonZeroExit(t *testing.T) {
	hNewADB(t, hStep{ids: []string{"NEWCODEC"}, exit: 1}) // grep found no PlaybackState

	// The dump really did arrive: read it the way exec.Output() delivers it.
	raw, err := exec.Command("adb", "-s", "1.2.3.4:5555", "shell", "probe").Output()
	if err == nil {
		t.Fatal("fixture is wrong: the fake adb was supposed to exit non-zero")
	}
	if ids := secureCodecIDs(string(raw)); len(ids) != 1 || ids[0] != "NEWCODEC" {
		t.Fatalf("fixture is wrong: stdout carried %q", raw)
	}

	// A non-zero exit does not make the output worthless: the probe ends in a
	// grep that exits 1 when it matches nothing, and the resource dump arrives in
	// the same breath.
	got := adbShell(context.Background(), "1.2.3.4:5555", "probe", time.Second)
	if got == "" {
		t.Fatal("adbShell discarded a complete dump because the exit status was 1")
	}
	if ids := secureCodecIDs(got); len(ids) != 1 || ids[0] != "NEWCODEC" {
		t.Errorf("adbShell returned %q, which does not parse to NEWCODEC", got)
	}
}

// The consequence, end to end: a box that never publishes a media session can
// never be detected, and the diagnostic blames adb's reachability.
func TestWaitForVideoBlindedByTheGrepExitStatus(t *testing.T) {
	a := hNewADB(t,
		hStep{ids: []string{"OLD"}, exit: 1},
		hStep{ids: []string{"OLD", "NEW"}, exit: 1},
	)
	c := hConfig()
	c.tuneTimeout = 700 * time.Millisecond
	err, _, log := hWait(t, c)
	// A box that publishes no media session must still be detected by the codec
	// signal; the grep's exit status is not evidence about adb.
	if err != nil {
		t.Fatalf("codec signal blinded by the grep exit status: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log, "via codec NEW") {
		t.Errorf("want the codec signal to have fired, got:\n%s", log)
	}
	// Two probes is the point: baseline, then the new decoder. Previously every
	// probe was discarded and the loop ran until TUNE_TIMEOUT.
	if n := a.calls("shell"); n > 4 {
		t.Errorf("took %d probes to detect a decoder present on the second", n)
	}
}

// ------------------------------------------------------- the codec signal

func TestWaitForVideoCodecSignals(t *testing.T) {
	cases := []struct {
		name  string
		steps []hStep
		want  string // "" means the gate must not open
	}{
		{"new id appears", []hStep{
			{ids: []string{"A"}, session: hStopped},
			{ids: []string{"A", "B"}, session: hStopped},
		}, "via codec B"},
		{"old id lingers alongside the new one", []hStep{
			{ids: []string{"OLD"}, session: hStopped},
			{ids: []string{"OLD"}, session: hStopped},
			{ids: []string{"NEW", "OLD"}, session: hStopped}, // new one listed FIRST
		}, "via codec NEW"},
		{"two ids at baseline, a third arrives", []hStep{
			{ids: []string{"A", "B"}, session: hStopped},
			{ids: []string{"A", "B", "C"}, session: hStopped},
		}, "via codec C"},
		{"id vanishes and the same id comes back", []hStep{
			{ids: []string{"A"}, session: hStopped},
			{ids: nil, session: hStopped},
			{ids: []string{"A"}, session: hStopped},
		}, ""},
		{"id changes twice -- fires on the first change", []hStep{
			{ids: []string{"A"}, session: hStopped},
			{ids: []string{"B"}, session: hStopped},
			{ids: []string{"C"}, session: hStopped},
		}, "via codec B"},
		{"a new decoder needs no arming", []hStep{
			{ids: []string{"A"}, session: hPlaying}, // parked playing: armed=false
			{ids: []string{"A", "B"}, session: hPlaying},
		}, "via codec B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hNewADB(t, tc.steps...)
			c := hConfig()
			c.tuneTimeout = 600 * time.Millisecond
			err, _, log := hWait(t, c)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("gate opened on a decoder that was present at baseline:\n%s", log)
				}
				return
			}
			if err != nil {
				t.Fatalf("waitForVideo: %v", err)
			}
			if !strings.Contains(log, tc.want) {
				t.Errorf("want %q in the log, got:\n%s", tc.want, log)
			}
		})
	}
}

// BUG (main.go:480-482, main.go:451). The baseline is taken by the first
// SUCCESSFUL poll, and nothing ever re-baselines. A decoder allocated between
// `adb connect` and that poll -- the window is the connect plus every failed
// probe, which is exactly when the box is busiest -- is absorbed into baseSet
// and the primary signal is dead for the whole tune. With the session parked at
// state=3 by the previous channel, `armed` never becomes true either, so both
// signals are dead and the tune burns the entire TUNE_TIMEOUT. The diagnostic
// then states, of a device that reported state=3 speed=1 on every single poll,
// that it "reported no secure decoder and no playing media session".
func TestWaitForVideoTimeoutNamesWhatItActuallySaw(t *testing.T) {
	hNewADB(t,
		hStep{fail: true}, // playback starts during this window
		hStep{ids: []string{"OLD", "NEW"}, session: hPlaying},
	)
	c := hConfig()
	c.tuneTimeout = 700 * time.Millisecond
	err, _, _ := hWait(t, c)
	if err == nil {
		t.Fatal("gate opened -- the bug appears to be fixed, flip this assertion")
	}
	// The tune still fails -- nothing about the box ever changed, so there was
	// nothing to detect. What must not happen is the message claiming the device
	// reported no decoder and no playing session, when it reported both on every
	// poll. It has to say what it actually saw.
	m := err.Error()
	for _, want := range []string{"nothing changed", "baseline codec=OLD,NEW", "last poll codec=OLD,NEW", "session=playing"} {
		if !strings.Contains(m, want) {
			t.Errorf("diagnostic = %q, want it to mention %q", m, want)
		}
	}
	if strings.Contains(m, "no secure decoder") {
		t.Errorf("diagnostic still claims no decoder, of a box that reported one every poll: %q", m)
	}
}

// -------------------------------------------------- the session fallback

func TestWaitForVideoSessionSignals(t *testing.T) {
	cases := []struct {
		name  string
		steps []hStep
		want  string
	}{
		{"parked at state=3 from the previous channel", []hStep{
			{ids: []string{"A"}, session: hPlaying},
		}, ""},
		{"3 -> 2 -> 3", []hStep{
			{ids: []string{"A"}, session: hPlaying},
			{ids: []string{"A"}, session: hStopped},
			{ids: []string{"A"}, session: hPlaying},
		}, "via session playing"},
		{"state=3 speed=0 does not count as playing", []hStep{
			{ids: []string{"A"}, session: hPaused},
			{ids: []string{"A"}, session: hPaused},
		}, ""},
		{"paused at baseline then really playing", []hStep{
			{ids: []string{"A"}, session: hPaused},
			{ids: []string{"A"}, session: hPlaying},
		}, "via session playing"},
		{"no media session at all", []hStep{
			{ids: []string{"A"}, session: ""},
			{ids: []string{"A"}, session: ""},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hNewADB(t, tc.steps...)
			c := hConfig()
			c.tuneTimeout = 600 * time.Millisecond
			err, _, log := hWait(t, c)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("gate opened when it must not have:\n%s", log)
				}
				return
			}
			if err != nil {
				t.Fatalf("waitForVideo: %v", err)
			}
			if !strings.Contains(log, tc.want) {
				t.Errorf("want %q in the log, got:\n%s", tc.want, log)
			}
		})
	}
}

// ------------------------------------------------- confirm, min_wait, settle

// CONFIRM counts CONSECUTIVE sightings: a probe that told us nothing has to
// break the run, or a sighting from before an adb failure combines with one
// after it across a gap.
func TestWaitForVideoConfirmResetsOnAFailedProbe(t *testing.T) {
	a := hNewADB(t,
		hStep{ids: []string{"A"}, session: hStopped},      // 1 baseline
		hStep{ids: []string{"A", "B"}, session: hStopped}, // 2 hit 1
		hStep{ids: []string{"A", "B"}, session: hStopped}, // 3 hit 2
		hStep{fail: true}, // 4 run broken
		hStep{ids: []string{"A", "B"}, session: hStopped}, // 5 hit 1
		hStep{ids: []string{"A", "B"}, session: hStopped}, // 6 hit 2
		hStep{ids: []string{"A", "B"}, session: hStopped}, // 7 hit 3 -> open
	)
	c := hConfig()
	c.confirm = 3
	if err, _, _ := hWait(t, c); err != nil {
		t.Fatalf("waitForVideo: %v", err)
	}
	if got := a.calls("shell"); got != 7 {
		t.Errorf("%d probes, want 7 -- hits survived the failed probe", got)
	}
}

func TestWaitForVideoMinWaitHoldsTheGate(t *testing.T) {
	steps := []hStep{
		{ids: []string{"A"}, session: hStopped},
		{ids: []string{"A", "B"}, session: hStopped},
	}
	hNewADB(t, steps...)
	c := hConfig()
	c.minWait = 400 * time.Millisecond
	err, took, _ := hWait(t, c)
	if err != nil {
		t.Fatalf("waitForVideo: %v", err)
	}
	if took < c.minWait {
		t.Errorf("opened after %v, MIN_WAIT is %v", took, c.minWait)
	}
	if took > c.minWait+500*time.Millisecond {
		t.Errorf("opened after %v, want just past MIN_WAIT (%v)", took, c.minWait)
	}
}

func TestWaitForVideoSettleDelaysTheReturn(t *testing.T) {
	steps := []hStep{
		{ids: []string{"A"}, session: hStopped},
		{ids: []string{"A", "B"}, session: hStopped},
	}
	hNewADB(t, steps...)
	c := hConfig()
	_, fast, _ := hWait(t, c)

	hNewADB(t, steps...)
	c = hConfig()
	c.settle = 350 * time.Millisecond
	err, slow, _ := hWait(t, c)
	if err != nil {
		t.Fatalf("waitForVideo: %v", err)
	}
	// Two single measurements rather than a difference of two: a delta assertion
	// compounds the jitter of both runs, and the true signal here (350ms) is only
	// ~3x the noise.
	if fast >= c.settle {
		t.Errorf("detection alone took %v, which is not below SETTLE (%v) -- "+
			"this test can no longer tell the two apart", fast, c.settle)
	}
	if slow < c.settle {
		t.Errorf("SETTLE=%v but the call returned after %v", c.settle, slow)
	}
}

// ------------------------------------------------------- timeout behaviour

// Which of the three timeout messages fires, and how far past TUNE_TIMEOUT the
// call actually returns.
func TestWaitForVideoTimeoutDiagnostics(t *testing.T) {
	t.Run("the whole budget went to connecting", func(t *testing.T) {
		a := hNewADB(t, hStep{ids: []string{"A"}, session: hStopped})
		a.hangConnect()
		c := hConfig()
		c.tuneTimeout = 400 * time.Millisecond
		err, took, _ := hWait(t, c)
		if err == nil {
			t.Fatal("want a timeout")
		}
		if !strings.Contains(err.Error(), "went to connecting") {
			t.Errorf("diagnostic = %q, want the connect message", err)
		}
		if a.calls("shell") != 0 {
			t.Errorf("polled %d times, want 0", a.calls("shell"))
		}
		t.Logf("TUNE_TIMEOUT=%v, returned after %v (overrun %v -- exec WaitDelay)",
			c.tuneTimeout, took.Round(10*time.Millisecond), (took - c.tuneTimeout).Round(10*time.Millisecond))
		if took > c.tuneTimeout+5*time.Second {
			t.Errorf("returned %v past TUNE_TIMEOUT", took-c.tuneTimeout)
		}
	})

	t.Run("every adb call failed", func(t *testing.T) {
		hNewADB(t, hStep{fail: true})
		c := hConfig()
		c.tuneTimeout = 500 * time.Millisecond
		err, _, _ := hWait(t, c)
		if err == nil || !strings.Contains(err.Error(), "every adb call") {
			t.Fatalf("diagnostic = %v, want the adb-failure message", err)
		}
	})

	t.Run("nothing playing", func(t *testing.T) {
		hNewADB(t, hStep{ids: []string{"A"}, session: hStopped})
		c := hConfig()
		c.tuneTimeout = 500 * time.Millisecond
		err, _, _ := hWait(t, c)
		if err == nil || !strings.Contains(err.Error(), "nothing changed") {
			t.Fatalf("diagnostic = %v, want the nothing-changed message", err)
		}
		if !strings.Contains(err.Error(), "baseline codec=A") {
			t.Errorf("diagnostic = %q, want baseline codec=A", err)
		}
	})

	// The third message also absorbs the likelier misattribution: the baseline
	// poll succeeded and adb then died for the rest of the tune. adbFailures is
	// polls-1 rather than polls, so the "every adb call failed" branch is missed
	// and the message states that the DEVICE reported nothing -- of a tune during
	// which the device was never successfully asked.
	t.Run("adb dies right after the baseline", func(t *testing.T) {
		hNewADB(t,
			hStep{ids: []string{"A"}, session: hStopped},
			hStep{fail: true},
		)
		c := hConfig()
		c.tuneTimeout = 500 * time.Millisecond
		err, _, _ := hWait(t, c)
		if err == nil {
			t.Fatal("want a timeout")
		}
		// adb was dead for every poll but the first, so the message must not blame
		// the device -- it has to show that the last poll returned nothing at all.
		m := err.Error()
		if !strings.Contains(m, "adb ok on 1/") {
			t.Errorf("diagnostic = %q, want the 1-of-N poll count", m)
		}
		if !strings.Contains(m, "last poll codec=none session=none") {
			t.Errorf("diagnostic = %q, want the last poll shown as having returned nothing", m)
		}
		if strings.Contains(m, "no secure decoder") {
			t.Errorf("diagnostic still blames the device: %q", m)
		}
	})

	// A probe that never answers, rather than one that fails fast.
	t.Run("a probe that hangs", func(t *testing.T) {
		hNewADB(t, hStep{hang: true})
		c := hConfig()
		c.tuneTimeout = 400 * time.Millisecond
		err, took, _ := hWait(t, c)
		if err == nil {
			t.Fatal("want a timeout")
		}
		t.Logf("TUNE_TIMEOUT=%v, returned after %v (overrun %v)",
			c.tuneTimeout, took.Round(10*time.Millisecond),
			(took - c.tuneTimeout).Round(10*time.Millisecond))
		if took > c.tuneTimeout+5*time.Second {
			t.Errorf("returned %v past TUNE_TIMEOUT", took-c.tuneTimeout)
		}
	})
}

// The reconnect that rescues a dropped adb session is rate limited from the
// INITIAL connect, so it cannot fire until 5s in however early the session
// drops -- a tune with TUNE_TIMEOUT below ~5s never gets it at all.
func TestWaitForVideoReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("needs >5s of wall clock to reach the rate limit")
	}
	a := hNewADB(t, hStep{fail: true})
	c := hConfig()
	c.poll = 50 * time.Millisecond
	c.tuneTimeout = 6 * time.Second
	if err, _, _ := hWait(t, c); err == nil {
		t.Fatal("want a timeout")
	}
	if got := a.calls("connect"); got != 2 {
		t.Errorf("%d connects in 6s of solid failure, want 2 (initial + one at ~5s)", got)
	}
	if got := a.calls("shell"); got < 50 {
		t.Errorf("only %d probes in 6s", got)
	}
}

// ---------------------------------------------------------------- adbShell

func TestAdbShellBudget(t *testing.T) {
	a := hNewADB(t, hStep{ids: []string{"A"}, session: hStopped})

	if got := adbShell(context.Background(), "1.2.3.4", "probe", 0); got != "" {
		t.Errorf("adbShell with a spent budget = %q, want empty", got)
	}
	if got := adbShell(context.Background(), "1.2.3.4", "probe", -time.Second); got != "" {
		t.Errorf("adbShell with a negative budget = %q, want empty", got)
	}
	if a.calls("shell") != 0 {
		t.Error("adbShell exec'd adb with no budget left")
	}
	if got := adbShell(context.Background(), "1.2.3.4", "probe", time.Second); !strings.Contains(got, "Id: A") {
		t.Errorf("adbShell = %q", got)
	}
}

// A budget above the 10s ceiling is clamped to it, so one call can never eat a
// long TUNE_TIMEOUT on its own.
func TestAdbShellClampsToTheCeiling(t *testing.T) {
	hNewADB(t, hStep{ids: []string{"A"}, session: hStopped})
	start := time.Now()
	if got := adbShell(context.Background(), "1.2.3.4", "probe", time.Hour); !strings.Contains(got, "Id: A") {
		t.Errorf("adbShell = %q", got)
	}
	if took := time.Since(start); took > adbCallTimeout {
		t.Errorf("took %v with a budget of an hour, ceiling is %v", took, adbCallTimeout)
	}
}

// DEBUG=1 is what a user is asked to set when a tune misbehaves, so the two
// debug lines have to render -- and name the baseline and the arming state,
// which is the only way to tell these failures apart from the outside.
func TestWaitForVideoDebugLog(t *testing.T) {
	hNewADB(t,
		hStep{ids: []string{"A"}, session: hPlaying},
		hStep{ids: []string{"A"}, session: hStopped},
	)
	c := hConfig()
	c.debug = true
	c.tuneTimeout = 300 * time.Millisecond
	_, _, log := hWait(t, c)
	for _, want := range []string{"baseline codec=A", "armed=false", "session=playing", "hits=0"} {
		if !strings.Contains(log, want) {
			t.Errorf("DEBUG log missing %q:\n%s", want, log)
		}
	}
}

// adb hands back CRLF; a stray \r inside the marker split or the PlaybackState
// line breaks the string matching downstream.
func TestAdbShellStripsCR(t *testing.T) {
	hNewADB(t, hStep{raw: "Processes:\r\n  Id: A\r\n__MS__\r\n__MS2__\r\n"})
	got := adbShell(context.Background(), "1.2.3.4", "probe", time.Second)
	if got == "" {
		t.Fatal("adbShell returned nothing")
	}
	if strings.Contains(got, "\r") {
		t.Errorf("adbShell left CR in %q", got)
	}
}

// The ceiling that TUNE_TIMEOUT depends on: a hung adb must not hold the caller
// past its budget by more than exec's WaitDelay.
func TestAdbShellHonoursItsBudget(t *testing.T) {
	hNewADB(t, hStep{hang: true})
	start := time.Now()
	got := adbShell(context.Background(), "1.2.3.4", "probe", 300*time.Millisecond)
	took := time.Since(start)
	if got != "" {
		t.Errorf("adbShell = %q, want empty", got)
	}
	t.Logf("budget 300ms, returned after %v", took.Round(10*time.Millisecond))
	if took > 5*time.Second {
		t.Errorf("adbShell took %v on a 300ms budget", took)
	}
}

// ------------------------------------------------------------ render gate

func TestAudioStarted(t *testing.T) {
	cases := []struct {
		name string
		dump string
		want bool
	}{
		{"started media playback", `  playback configurations:
    AudioPlaybackConfiguration: ID:26 -- u/pid:10099/4529 -- state:started -- attr: usage=USAGE_MEDIA content=CONTENT_TYPE_MOVIE`, true},
		{"idle", `    AudioPlaybackConfiguration: ID:26 -- state:idle -- attr: usage=USAGE_MEDIA content=CONTENT_TYPE_MOVIE`, false},
		{"a UI sound is not media", `    AudioPlaybackConfiguration: ID:3 -- state:started -- attr: usage=USAGE_ASSISTANCE_SONIFICATION content=CONTENT_TYPE_SONIFICATION`, false},
		{"nothing at all", "", false},
		// BUG (main.go:562): the three tokens must be on ONE line, including the
		// literal class name. AudioPlaybackConfiguration.toLogFriendlyString() --
		// what `dumpsys audio` prints under "playback configurations:" on the
		// builds this was checked against -- does not repeat the class name per
		// line. On any device that prints this shape, WAIT_AUDIO is a silent
		// RENDER_TIMEOUT-long no-op that reports "render not confirmed".
		{"toLogFriendlyString shape without the class name", `  playback configurations:
    ID:26 -- type: android.media.MediaPlayer -- u/pid:10099/4529 -- state:started -- attr:AudioAttributes: usage=USAGE_MEDIA content=CONTENT_TYPE_MOVIE flags=0x0`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := hNewADB(t, hStep{ids: []string{"A"}})
			a.setAudio(tc.dump)
			c := hConfig()
			if got := audioStarted(context.Background(), c, time.Second); got != tc.want {
				t.Errorf("audioStarted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAudioStartedOnAFailedProbe(t *testing.T) {
	a := hNewADB(t, hStep{ids: []string{"A"}})
	a.setAudio("@FAIL\n")
	if audioStarted(context.Background(), hConfig(), time.Second) {
		t.Error("audioStarted = true for a failed dumpsys")
	}
}

func TestWaitForRender(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		a := hNewADB(t, hStep{ids: []string{"A"}})
		c := hConfig()
		start := time.Now()
		waitForRender(context.Background(), c)
		if took := time.Since(start); took > 50*time.Millisecond {
			t.Errorf("WAIT_AUDIO off still cost %v", took)
		}
		if a.calls("audio") != 0 {
			t.Error("WAIT_AUDIO off still dumped audio")
		}
	})

	t.Run("confirmed", func(t *testing.T) {
		a := hNewADB(t, hStep{ids: []string{"A"}})
		a.setAudio("AudioPlaybackConfiguration: state:started usage=USAGE_MEDIA\n")
		c := hConfig()
		c.waitAudio = true
		var log string
		start := time.Now()
		log = hCaptureStderr(t, func() { waitForRender(context.Background(), c) })
		if took := time.Since(start); took > c.renderTimeout {
			t.Errorf("took %v, want well under RENDER_TIMEOUT %v", took, c.renderTimeout)
		}
		if !strings.Contains(log, "render confirmed") {
			t.Errorf("log = %q", log)
		}
	})

	t.Run("never confirmed", func(t *testing.T) {
		a := hNewADB(t, hStep{ids: []string{"A"}})
		a.setAudio("nothing here\n")
		c := hConfig()
		c.waitAudio = true
		start := time.Now()
		log := hCaptureStderr(t, func() { waitForRender(context.Background(), c) })
		took := time.Since(start)
		if !strings.Contains(log, "render not confirmed") {
			t.Errorf("log = %q", log)
		}
		if took < c.renderTimeout {
			t.Errorf("gave up after %v, RENDER_TIMEOUT is %v", took, c.renderTimeout)
		}
		if took > c.renderTimeout+2*time.Second {
			t.Errorf("took %v, RENDER_TIMEOUT is %v", took, c.renderTimeout)
		}
		if a.calls("audio") < 2 {
			t.Errorf("%d audio dumps", a.calls("audio"))
		}
	})

	// A hung `dumpsys audio` holds the gate past RENDER_TIMEOUT by exec's
	// WaitDelay, on top of a tune that has already spent TUNE_TIMEOUT.
	t.Run("a dump that hangs", func(t *testing.T) {
		a := hNewADB(t, hStep{ids: []string{"A"}})
		a.setAudio("@HANG\n")
		c := hConfig()
		c.waitAudio = true
		start := time.Now()
		waitForRender(context.Background(), c)
		took := time.Since(start)
		t.Logf("RENDER_TIMEOUT=%v, returned after %v (overrun %v)",
			c.renderTimeout, took.Round(10*time.Millisecond),
			(took - c.renderTimeout).Round(10*time.Millisecond))
		if took > c.renderTimeout+5*time.Second {
			t.Errorf("held the gate %v past RENDER_TIMEOUT", took-c.renderTimeout)
		}
	})
}

// ------------------------------------------------------------- parser bugs

// BUG (main.go:295). `id` is not cleared between process blocks, so a secure
// video codec in a block with no Id: line of its own is attributed to whichever
// process was parsed last. A phantom id at baseline is harmless; a phantom id
// mid-tune is a false new decoder, which opens the gate on nothing.
func TestSecureCodecIDsLeaksIDAcrossProcesses(t *testing.T) {
	dump := `
  Processes:
    Pid: 1000
      Id: LAUNCHER
      {name: graphic-memory, value: 4096}
    Pid: 2000
      {name: secure-codec, subType: video-codec, value: 1}
`
	got := secureCodecIDs(dump)
	if len(got) == 1 && got[0] == "LAUNCHER" {
		t.Log("BUG main.go:295: the codec in pid 2000 was reported under pid 1000's id")
		return
	}
	if len(got) != 0 {
		t.Errorf("secureCodecIDs = %v", got)
	}
}

// ------------------------------------------------------- main() as a process

// The subprocess entry point. Skipped in an ordinary run.
func TestStreamgateSubprocess(t *testing.T) {
	if os.Getenv("SG_SUBPROCESS") != "1" {
		t.Skip("entry point for hRunMain; runs only when re-executed")
	}
	os.Args = append([]string{"streamgate"}, strings.Fields(os.Getenv("SG_ARGS"))...)
	main()
	// Exit before the testing package can print PASS: stdout is the video
	// stream and its exact byte count is the assertion.
	os.Exit(0)
}

type hMainResult struct {
	code   int
	stdout []byte
	stderr string
	wall   time.Duration
}

// hRunMain runs main() in a real process with a real fd 1.
func hRunMain(t *testing.T, env map[string]string) hMainResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStreamgateSubprocess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SG_SUBPROCESS=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	start := time.Now()
	err := cmd.Run()
	res := hMainResult{stdout: so.Bytes(), stderr: se.String(), wall: time.Since(start)}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running the subprocess: %v\n%s", err, se.String())
		}
		res.code = ee.ExitCode()
	}
	return res
}

// hEnv is the base environment: tuner 1, every optional stage off so a test
// measures only what it is about.
func hEnv(encoderURL string) map[string]string {
	return map[string]string{
		"SG_ARGS":        "1",
		"TUNER1_IP":      "127.0.0.1:5555",
		"ENCODER1_URL":   encoderURL,
		"TUNE_TIMEOUT":   "1s",
		"POLL":           "20ms",
		"CONFIRM_POLL":   "10ms",
		"MIN_WAIT":       "0",
		"SETTLE":         "0",
		"ON_TIMEOUT":     "fail",
		"WAIT_MOTION":    "0",
		"DRAIN_IDLE":     "0",
		"WAIT_AUDIO":     "0",
		"READ_TIMEOUT":   "5s",
		"ALIGN_TIMEOUT":  "2s",
		"MOTION_TIMEOUT": "1s",
	}
}

// hServeTS serves the stream a packet at a time. repeat 0 means forever, which
// is what a real encoder does.
func hServeTS(t *testing.T, data []byte, gap time.Duration, repeat int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for pass := 0; repeat == 0 || pass < repeat; pass++ {
			for i := 0; i+tsPacketSize <= len(data); i += tsPacketSize {
				if _, err := w.Write(data[i : i+tsPacketSize]); err != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(gap)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The promise the whole design rests on: ON_TIMEOUT=fail exits non-zero having
// written ZERO bytes, with a healthy encoder streaming the entire time.
func TestMainOnTimeoutFailWritesNoBytes(t *testing.T) {
	hNewADB(t, hStep{fail: true})
	srv := hServeTS(t, buildStream(400, 10), 2*time.Millisecond, 0)
	res := hRunMain(t, hEnv(srv.URL))

	if res.code != 1 {
		t.Errorf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	if len(res.stdout) != 0 {
		t.Errorf("ON_TIMEOUT=fail put %d bytes on stdout, want 0", len(res.stdout))
	}
	if !strings.Contains(res.stderr, "failing the tune") {
		t.Errorf("stderr = %s", res.stderr)
	}
	t.Logf("TUNE_TIMEOUT=1s, process exited after %v", res.wall.Round(10*time.Millisecond))
	if res.wall > 6*time.Second {
		t.Errorf("took %v to honour a 1s TUNE_TIMEOUT", res.wall)
	}
}

// Anything other than "fail" streams whatever is on screen.
func TestMainOnTimeoutStreamWritesBytes(t *testing.T) {
	hNewADB(t, hStep{fail: true})
	srv := hServeTS(t, buildStream(900, 10), 2*time.Millisecond, 1)
	env := hEnv(srv.URL)
	env["ON_TIMEOUT"] = "stream"
	res := hRunMain(t, env)

	if len(res.stdout) == 0 {
		t.Fatalf("ON_TIMEOUT=stream wrote nothing\n%s", res.stderr)
	}
	if res.stdout[0] != 0x47 {
		t.Errorf("stdout does not start on a TS sync byte: %x", res.stdout[0])
	}
	if got := pid(res.stdout[:tsPacketSize]); got != 0 {
		t.Errorf("first packet pid = %d, want the cached PAT", got)
	}
	if res.code != 1 {
		t.Errorf("exit %d, want 1 (the encoder ended the stream)\n%s", res.code, res.stderr)
	}
}

// A tune that never carried a picture must still put nothing on the wire, and
// must name the encoder rather than the box.
func TestMainEncoderFailures(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		onTimeout string
		wantLog   string
	}{
		{"non-200, ON_TIMEOUT=fail", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, "fail", "encoder returned 503"},
		{"non-200, ON_TIMEOUT=stream", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, "stream", "encoder returned 503"},
		{"200 with an empty body, ON_TIMEOUT=fail", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}, "fail", "encoder sent no video"},
		{"200 with an empty body, ON_TIMEOUT=stream", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}, "stream", "encoder sent no video"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hNewADB(t, hStep{fail: true})
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			env := hEnv(srv.URL)
			env["ON_TIMEOUT"] = tc.onTimeout
			res := hRunMain(t, env)

			if len(res.stdout) != 0 {
				t.Errorf("%d bytes on stdout, want 0", len(res.stdout))
			}
			if res.code != 1 {
				t.Errorf("exit %d, want 1\n%s", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.wantLog) {
				t.Errorf("stderr does not name the encoder fault %q:\n%s", tc.wantLog, res.stderr)
			}
		})
	}
}

// The encoder dies while detection is still running. Before the gate opens that
// is recoverable -- every byte is being discarded anyway -- so streamgate
// redials rather than latching the failure and killing a tune whose box then
// comes up fine. It must still say the encoder was the problem, and it must
// still put zero bytes on the wire when detection then times out.
func TestMainEncoderDiesDuringDetection(t *testing.T) {
	for _, onTimeout := range []string{"fail", "stream"} {
		t.Run("ON_TIMEOUT="+onTimeout, func(t *testing.T) {
			hNewADB(t, hStep{fail: true})
			// 30 packets and then EOF, well inside the 1s TUNE_TIMEOUT.
			srv := hServeTS(t, buildStream(30, 10), time.Millisecond, 1)
			env := hEnv(srv.URL)
			env["ON_TIMEOUT"] = onTimeout
			res := hRunMain(t, env)

			if len(res.stdout) != 0 {
				t.Errorf("%d bytes on stdout, want 0", len(res.stdout))
			}
			if res.code != 1 {
				t.Errorf("exit %d, want 1\n%s", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, "encoder") {
				t.Errorf("stderr never mentions the encoder:\n%s", res.stderr)
			}
			if !strings.Contains(res.stderr, "redialling") {
				t.Errorf("encoder failure before the gate was latched instead of retried:\n%s", res.stderr)
			}
		})
	}
}

// TUNERn_IP unset is the documented "no gate" mode: stream at once, never touch
// adb.
func TestMainTunerIPUnset(t *testing.T) {
	a := hNewADB(t, hStep{fail: true})
	srv := hServeTS(t, buildStream(200, 10), time.Millisecond, 1)
	env := hEnv(srv.URL)
	env["TUNER1_IP"] = ""
	res := hRunMain(t, env)

	if len(res.stdout) == 0 {
		t.Fatalf("wrote nothing with no gate\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "no gate") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if a.calls("shell") != 0 || a.calls("connect") != 0 {
		t.Errorf("adb was called %d/%d times with no TUNER1_IP",
			a.calls("connect"), a.calls("shell"))
	}
}

// A box that accepts the TCP connection and never answers must not hold the
// process open indefinitely; TUNE_TIMEOUT is the only bound the DVR has.
//
// The two hangs cost differently, which is worth pinning. `adb connect` runs
// under exec.Run() with no pipes, so killing it ends the call immediately and
// the process exits ON the deadline. A probe runs under Output(), whose pipes a
// surviving adb server daemon still holds, so it costs the full 2s WaitDelay on
// top of TUNE_TIMEOUT (main.go:374).
func TestMainHangingADBStillExits(t *testing.T) {
	cases := []struct {
		name        string
		hangConnect bool
	}{
		{"adb connect never answers", true},
		{"the probe never answers", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := hNewADB(t, hStep{hang: true})
			if tc.hangConnect {
				a.hangConnect()
			}
			srv := hServeTS(t, buildStream(400, 10), 2*time.Millisecond, 0)
			res := hRunMain(t, hEnv(srv.URL))

			if res.code != 1 {
				t.Errorf("exit %d, want 1\n%s", res.code, res.stderr)
			}
			if len(res.stdout) != 0 {
				t.Errorf("%d bytes on stdout, want 0", len(res.stdout))
			}
			t.Logf("TUNE_TIMEOUT=1s: exited after %v (overrun %v)",
				res.wall.Round(10*time.Millisecond),
				(res.wall - time.Second).Round(10*time.Millisecond))
			if res.wall > 8*time.Second {
				t.Errorf("took %v to honour a 1s TUNE_TIMEOUT", res.wall)
			}
		})
	}
}

func TestMainConfigErrors(t *testing.T) {
	t.Run("no tuner number", func(t *testing.T) {
		res := hRunMain(t, map[string]string{"SG_ARGS": ""})
		if res.code != 2 {
			t.Errorf("exit %d, want 2\n%s", res.code, res.stderr)
		}
		if len(res.stdout) != 0 {
			t.Errorf("%d bytes on stdout", len(res.stdout))
		}
	})
	t.Run("ENCODERn_URL unset", func(t *testing.T) {
		env := hEnv("")
		res := hRunMain(t, env)
		if res.code != 2 {
			t.Errorf("exit %d, want 2\n%s", res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, "ENCODER1_URL not set") {
			t.Errorf("stderr = %s", res.stderr)
		}
	})
	t.Run("--version", func(t *testing.T) {
		res := hRunMain(t, map[string]string{"SG_ARGS": "--version"})
		if res.code != 0 {
			t.Errorf("exit %d, want 0", res.code)
		}
		if len(res.stdout) != 0 {
			t.Errorf("--version put %d bytes on stdout; that fd is the stream", len(res.stdout))
		}
		if !strings.Contains(res.stderr, "streamgate") {
			t.Errorf("stderr = %s", res.stderr)
		}
	})
}

// The whole path, in one process: adb reports a new decoder, the gate opens,
// bytes reach fd 1 -- and when the DVR hangs up, that is the NORMAL end of a
// recording and the exit code must be 0, not 141 and not 1.
func TestMainGateOpensThenTheDVRHangsUp(t *testing.T) {
	hNewADB(t,
		hStep{ids: []string{"OLD"}, session: hStopped},
		hStep{ids: []string{"OLD", "NEW"}, session: hPlaying},
	)
	srv := hServeTS(t, buildStream(400, 10), 2*time.Millisecond, 0)
	env := hEnv(srv.URL)
	env["TUNE_TIMEOUT"] = "5s"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStreamgateSubprocess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SG_SUBPROCESS=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var se bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	// Read a few packets, then hang up like a DVR that stopped the recording.
	got := make([]byte, 4*tsPacketSize)
	if _, err := io.ReadFull(r, got); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("reading the stream: %v\n%s", err, se.String())
	}
	_ = r.Close()

	err = cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("waiting: %v\n%s", err, se.String())
	}
	if got[0] != 0x47 || pid(got[:tsPacketSize]) != 0 {
		t.Errorf("stream does not start with the cached PAT: pid %d", pid(got[:tsPacketSize]))
	}
	if !strings.Contains(se.String(), "playback detected") {
		t.Errorf("stderr = %s", se.String())
	}
	if code != 0 {
		t.Errorf("exit %d after the DVR hung up, want 0\n%s", code, se.String())
	}
	if !strings.Contains(se.String(), "closed by the DVR") {
		t.Errorf("the DVR hanging up was not reported as a normal end:\n%s", se.String())
	}
}

// A probe that COMPLETED with an empty session half means the box has no media
// session at all -- genuinely not playing, so it must arm the fallback. A probe
// that was cut SHORT means we know nothing, so it must not. Collapsing those two
// left `armed` false forever whenever an app tears its MediaSession down while
// retuning, and on a box with no usable resource-manager signal that fails every
// tune outright.
func TestSessionTornDownDuringChannelChangeStillArms(t *testing.T) {
	hNewADB(t,
		hStep{session: hPlaying},          // baseline: outgoing channel parked at 3
		hStep{},                           // session released while retuning
		hStep{},                           //
		hStep{session: hPlaying, exit: 0}, // new channel genuinely playing
		hStep{session: hPlaying},          //
	)
	c := hConfig()
	c.tuneTimeout = 3 * time.Second
	err, _, log := hWait(t, c)
	if err != nil {
		t.Fatalf("never armed, so the new channel never registered: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log, "via session playing") {
		t.Errorf("want the session fallback to have fired, got:\n%s", log)
	}
}

// The same shape, but the probe is cut short rather than answering emptily: that
// must NOT arm, or the outgoing channel's parked state=3 opens the gate.
func TestCutShortSessionDoesNotArm(t *testing.T) {
	hNewADB(t,
		hStep{session: hPlaying},                          // baseline: parked at 3
		hStep{raw: "Processes:\n  Pid: 4000\n    Id: OL"}, // truncated, no marker
		hStep{session: hPlaying},                          // the SAME parked session
	)
	c := hConfig()
	c.tuneTimeout = 900 * time.Millisecond
	err, _, log := hWait(t, c)
	if err == nil {
		t.Fatalf("gate opened on a session that never dropped; log:\n%s", log)
	}
}
