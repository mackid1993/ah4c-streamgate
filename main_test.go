package main

import (
	"os"
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
		{"10s", 10 * time.Second},        // Go duration syntax
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
}

// An in-place channel switch leaves the previous decoder allocated alongside the
// new one, listed first. Returning the first match reported the OLD id, so the
// "changed id" test never fired and the tune timed out.
func TestSecureCodecIDPrefersNewest(t *testing.T) {
	dump := `
  Processes:
    Pid: 1000
      Id: OLDCODEC
      {name: secure-codec, subType: video-codec, value: 1}
    Pid: 2000
      Id: NEWCODEC
      {name: secure-codec, subType: video-codec, value: 1}
`
	if got := secureCodecID(dump); got != "NEWCODEC" {
		t.Errorf("secureCodecID = %q, want NEWCODEC", got)
	}
}

func TestSecureCodecIDRealFormat(t *testing.T) {
	dump := `
  Processes:
    Pid: 4529
      Id: 1284494944
      {name: secure-codec, subType: video-codec, value: 1}
`
	if got := secureCodecID(dump); got != "1284494944" {
		t.Errorf("secureCodecID = %q, want 1284494944", got)
	}
}

// A non-DRM app allocates a NON-secure decoder; it must not be mistaken for one.
func TestSecureCodecIDIgnoresNonSecure(t *testing.T) {
	dump := `
  Processes:
    Pid: 1000
      Id: PLAIN
      {name: non-secure-codec, subType: video-codec, value: 1}
`
	if got := secureCodecID(dump); got != "" {
		t.Errorf("secureCodecID = %q, want empty for a non-secure decoder", got)
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
