# ah4c-streamgatego

A gate for [ah4c](https://github.com/sullrich/ah4c) that waits until your Android box is **actually playing video** before handing the encoder stream to your DVR.

Without it, recording starts the moment the tuner is reserved — so the first several seconds of every tune are a splash screen, a loading spinner, or the channel you were on before. streamgate watches the box over adb, waits for real playback, and only then opens the pipe.

The video is never re-encoded. The encoder's bytes go through untouched.

> The original POSIX shell implementation lives on the [`bash`](../../tree/bash) branch and still works. This branch is a single static binary that does the same job plus two things a shell script structurally cannot: hold the encoder connection open during the wait, and start the output on a keyframe.

---

## Install

Download a binary from [Releases](../../releases) — `linux-amd64`, `linux-arm64`, or `linux-armv7`. They're statically linked, so there's nothing to install alongside them. They do call `adb`, which the ah4c image already ships.

```sh
curl -fsSL -o /path/to/ah4c/scripts/streamgate \
  https://github.com/mackid1993/ah4c-streamgatego/releases/latest/download/streamgate-linux-amd64
chmod +x /path/to/ah4c/scripts/streamgate
```

That directory is the one you already bind-mount for ah4c; inside the container it's `/opt/scripts`. Then point each tuner's `CMD` at it, with the tuner number as the only argument:

```
CMD1=/opt/scripts/streamgate 1
CMD2=/opt/scripts/streamgate 2
```

Restart the container. `TUNERn_IP` and `ENCODERn_URL` are read from the environment — there's nothing else to configure, and the defaults are meant to be good.

Check both spellings. A missing `ENCODERn_URL` exits immediately, but a missing or misspelled `TUNERn_IP` is **not** fatal: that tuner logs `TUNERn_IP not set -- no gate` once and then streams ungated, so it records the head of the previous channel on every tune, indefinitely, with no other complaint.

---

## How `CMDn` works in ah4c

ah4c has two ways to source a tuner's video.

**Without `CMDn`** it fetches `ENCODERn_URL` itself and copies those bytes to your DVR.

**With `CMDn`** it runs your command instead and pipes **that command's stdout** to the DVR. Whatever the command writes *is* the stream. If it exits without writing anything, the tune fails.

That's the hook: streamgate withholds stdout while it watches the box, then streams the encoder once playback is real.

Two details worth knowing:

- **ah4c does not run `CMDn` through a shell.** It splits on spaces and execs directly — no `$VAR` expansion, no pipes. A binary plus a number is the clean form.
- **`CMDn` runs concurrently with your tune script.** ah4c starts the command, then fires `bmitune.sh` when the DVR first reads. streamgate is already watching before the tune lands, which is deliberate — it snapshots what the *previous* channel was doing so it can tell that apart from the new one.

---

## How it decides playback is real

Two signals, cheapest first, both from a single adb round trip per poll.

**1. A new secure video decoder.** `dumpsys media.resource_manager` lists which processes hold a video codec. Android issues a new client id per playback session, so streamgate records the **set** of ids before tuning and waits for one that wasn't in it.

That distinction matters more than it looks. Asking whether a decoder merely *exists* returns true immediately, because the previous channel's decoder is often still allocated when the gate starts — an in-place channel switch tears nothing down. Comparing sets rather than picking one id also means it doesn't matter what order the device lists them in, which varies.

**2. The media session.** `dumpsys media_session` reporting `state=3` with `speed=1`. Used when the first signal isn't available — older Android, or vendor builds that report codecs differently.

This one has no identity, so it only counts once it has dropped at least once since the gate started. A channel that just ended can leave its session parked at exactly `state=3`, and without that rule it would read as instant success.

Whichever fires must hold for `CONFIRM` consecutive polls.

Both rest on the baseline being real, so the baseline is taken by the first poll that actually comes back — not by one unchecked call at startup. An empty dump makes every decoder look new and reads as "not playing", so a single failed probe (and the call right after `adb connect` is the likeliest one to fail) would otherwise open the gate immediately on the channel you're leaving. If the adb session drops mid-wait, streamgate reconnects rather than spending the rest of `TUNE_TIMEOUT` failing.

---

## Starting on a keyframe

This is the part that isn't obvious, and it's where most of the tune-to-tune inconsistency lives.

H.264 in an MPEG-TS stream is mostly P and B frames, which are differences against earlier frames. Only an **IDR keyframe** stands alone. If you attach to a live stream partway between keyframes, the decoder has to throw everything away until the next one arrives.

Encoders emit keyframes on a fixed cycle — every 2 seconds is typical. Attach at a random moment and you wait anywhere from zero to a full cycle, redrawn every tune:

```
IDR ......... IDR ......... IDR
     ^ attach here → nearly a full GOP of nothing
              ^ attach here → picture almost immediately
```

That's why the same box on the same channel can feel instant once and sluggish the next time.

So when the gate opens, streamgate skips forward to the next keyframe and starts there, prefixed with the most recent PAT and PMT it saw, so your DVR can parse the stream immediately instead of waiting for the encoder's next table cycle.

**It only ever skips forward.** It never emits a byte received before the gate opened, so it cannot introduce a channel-change banner, a tuning prompt, or a transition frame that a plain byte-copy wouldn't also have shown you.

If your encoder doesn't mark keyframes in a way it recognises, it gives up after `ALIGN_TIMEOUT` and streams normally rather than sitting there discarding forever.

### Waiting for the picture to actually move

Aligning to a keyframe is not enough on its own, because the app's loading
screen is *also* made of keyframes. DirecTV shows a blue card with the channel
logo; other apps show a spinner or an intro animation. The video decoder is
allocated while that is still on screen, so the first keyframe after the gate
opens can easily be the card — and you record it.

So before releasing, streamgate waits for the picture to start moving.

It works this out from the stream itself, with no extra cost, because it is
already reading every byte. A still card compresses to almost nothing; moving
video does not. It measures how much data each short window carries, remembers
the quietest window it has seen, and calls it motion when a window rises well
above that floor **and stays there** — a single spike is not enough, since the
cut itself and any animation produce brief ones.

**Nothing is compared against a fixed bitrate.** Absolute numbers are
meaningless across encoders, resolutions and quality settings — one channel here
runs real programming at 2,900 kbps where another runs 6,900. The floor is
learned per stream, and the test is a ratio, so it travels.

- **Warm tune, programming already flowing** — usually motion is already present
  and the next keyframe is released immediately. Not guaranteed: the test is a
  ratio to the quietest window ever seen, so a uniformly busy stream that never
  dips has no rise to find and pays the full `MOTION_TIMEOUT`. The cost per tune
  is anywhere between zero and that, depending on the content.
- **Cold box, or an app that cold-starts with an intro** — the gate holds until
  the picture moves, then releases on the next keyframe. Only tunes that need it
  pay for it.

It is bounded at every stage, because the failure modes matter more than the
optimisation:

- No rise within `MOTION_TIMEOUT` — release anyway. Covers a constant-bitrate
  encoder, where there is no rise to detect, and a box that was never slept so
  programming was already running before we connected.
- No keyframe within `ALIGN_TIMEOUT` + 2s — stream unaligned rather than produce no output at all. The cached PAT/PMT are still sent first, so the DVR does not additionally wait for the encoder's next table cycle.

One thing the floor cannot tell you: everything measured *before* the gate came off a different picture, usually the channel you're leaving. If that channel was already playing, motion may register on it and wave the new channel's loading card straight through. `REARM_MOTION=1` throws that away and re-learns from the gate. It's off by default because a switch that never shows a card — a retune to what's already playing — then has no rise to find and pays the whole `MOTION_TIMEOUT`. Turn it on if you see the card and leave it off otherwise.

## Settings

All optional, all environment variables, set on the **ah4c container** (streamgate inherits its environment). Durations accept either seconds (`5`, `0.25`) or Go syntax (`10s`, `250ms`). A value that can't be parsed is ignored with a warning on stderr rather than silently falling back.

Note these apply to **every tuner**. If you need one tuner to differ — say a different app with a longer intro — set it on that tuner's command instead:

```
CMD6=/usr/bin/env MOTION_TIMEOUT=12s /opt/scripts/streamgate 6
```

The defaults are tuned; most people should never touch these.

| Variable | Default | What it does |
|---|---|---|
| `CONFIRM` | `1` | Consecutive sightings required before handing off. Raise to `2`+ if a splash frame slips through. |
| `POLL` | `0.25` | Seconds between polls while waiting for a first sighting. |
| `CONFIRM_POLL` | `0.05` | Seconds between polls once something has been sighted. Confirming is on the critical path, so it polls tight. |
| `SETTLE` | `0.25` | Pause after detecting, before opening the gate. `0` skips it. See "if a recording starts on the app's tuning screen" below. |
| `MIN_WAIT` | `1` | Ignore "playing" for this many seconds after start. `0` accepts a sighting on the first poll. |
| `TUNE_TIMEOUT` | `40` | Give up after this long. Costs nothing on a tune that works — the wait ends the moment playback is detected. |
| `ON_TIMEOUT` | `fail` | `fail` exits without streaming, so your DVR sees a dead tune and can retry or pick another tuner. Anything else streams whatever is on screen. |
| `ALIGN_KEYFRAME` | `1` | Start output on a keyframe. `0` streams from wherever the encoder happens to be — and because the motion gate runs while waiting to align, `0` turns `WAIT_MOTION` off too. |
| `ALIGN_TIMEOUT` | `8` | If no keyframe is recognised within this long, stream unaligned rather than stall. Automatically raised above `MOTION_TIMEOUT` if you set them so they'd conflict. |
| `WAIT_AUDIO` | `0` | After the decoder appears, also wait for audio playback to start. Costs ~0.7s. Only needed if a flash survives `SETTLE`. |
| `RENDER_TIMEOUT` | `3` | Cap on that wait, so a device that never reports audio still tunes. |
| `WAIT_MOTION` | `1` | Wait for the picture to start moving before releasing. `0` releases on the first keyframe. |
| `MOTION_WINDOW` | `0.25` | Seconds per measurement window. |
| `DRAIN_IDLE` | `500us` | At handoff, discard video the encoder sent while we were still waiting on the box, so playback starts from live rather than from whatever had queued up. A read faster than this came from a buffer, not the network. `0` disables it. **Note the unit:** this is the only setting here measured in microseconds, and a bare number is read as seconds — `DRAIN_IDLE=1` means one second, which would discard the head of every recording. Catching up is also capped at 2s regardless. |
| `MOTION_HOLD` | `3` | Consecutive windows above the threshold before it counts as motion. Filters out brief spikes from the cut itself. |
| `RISE_FACTOR` | `5` | How far above the quietest observed window a window must rise. A ratio, not a bitrate. |
| `MOTION_TIMEOUT` | `6` | Give up waiting for motion after this long and release anyway. |
| `REARM_MOTION` | `0` | Discard what the motion detector learned before the gate and re-learn from it. Closes the case where the previous channel's motion releases the new channel's loading card, at the cost of `MOTION_TIMEOUT` on switches that never show a card. |
| `READ_TIMEOUT` | `10` | Give up if the encoder holds the connection open but sends nothing for this long. Catches a lost HDMI input or a wedged encode thread, which TCP keepalive does not — the peer is still answering. Every successful read pushes it out, so it only fires on genuine silence. |
| `DEBUG` | unset | Log every poll. |

### If a recording starts on the app's tuning screen

First, see the section above — on most encoders the scene-change keyframe handles this for you, and there is nothing to tune.

If yours doesn't, you have two options, in order:

**Raise `SETTLE`.** The decoder is allocated a little before the app puts real video on screen. `SETTLE` is a flat pause covering that gap; `0.5` or `0.75` is a reasonable next step. Cheap, but it's a timer — it doesn't know what's on screen, so it can be too short on a slow tune and wasted time on a fast one.

**Or set `WAIT_AUDIO=1`.** This waits for the app to actually start audio playback before opening the gate. It's the stronger signal for exactly this problem: **a tuning card doesn't play audio.** An audio track reaching the started state means the stream itself is decoding, not merely that a decoder object exists. It costs the real time the box takes to begin playback — roughly 0.7s on the hardware this was developed against — and it is bounded by `RENDER_TIMEOUT` so a device that never reports audio still tunes.

---

## Logs

Every tune logs one detection line and one alignment line:

```
streamgate[1]: playback detected after 5s via codec 1284494944 (base none), 1 confirmation(s)
streamgate[1]: aligned video-pid=100 discarded=159 packets/29KB caught-up=0KB gate-to-air=0.21s waited-for-motion=180ms picture=2903kbps still-picture-floor=342kbps ratio=8.5x keyframes-skipped=0
```

What each number means:

| field | meaning |
|---|---|
| `playback detected after Ns` | time from the tuner being reserved until a decoder appeared. Mostly the box and the app; not much to do with this program. |
| `via codec … (base …)` | which signal fired. `base` is what was allocated before tuning — a *changed* id is the proof of new playback. |
| `video-pid` | the PID carrying the keyframe the stream started on. |
| `discarded` | bytes dropped between the gate opening and that keyframe. These would have been undecodable to your DVR anyway. |
| `caught-up` | bytes thrown away by `DRAIN_IDLE` to get back to live. Normally a KB or two — streamgate reads the encoder continuously while it waits, so there is rarely much of a backlog to clear. A large number here means your encoder buffers, and `DRAIN_IDLE` is earning its keep. |
| `gate-to-air` | **the number to watch.** Total time from the gate opening to the first byte sent. |
| `waited-for-motion` | how much of `gate-to-air` was spent waiting for the picture to start moving. Small means the tune cost nothing extra; a second or more means it genuinely held through a loading screen. |
| `picture` / `still-picture-floor` | current data rate versus the quietest the stream has been. The floor is normally the app's loading screen. Windows that carry almost nothing are ignored, so a gap in the encoder's output cannot masquerade as a very quiet picture and make the card itself look like motion. |
| `ratio` | how many times above the floor. Release needs `RISE_FACTOR` (default 5×) sustained for `MOTION_HOLD` windows. |
| `keyframes-skipped` | keyframes passed over while waiting. Non-zero means the loading screen outlived a full GOP. |

A box that sleeps between tunes always shows a static picture before the gate
opens, so the motion detector nearly always trips *after* it — including when it
trips 200ms later and the tune is effectively instant. `waited-for-motion` is
what separates those cases, not the mere fact that it tripped.

When the gate can't confirm anything:

```
streamgate[1]: aligned video-pid=100 discarded=412 packets/76KB caught-up=0KB gate-to-air=6.05s waited-for-motion=6s(timeout, released anyway) keyframes-skipped=2
streamgate[1]: no keyframe recognised within 10s; streaming unaligned (encoder may not signal random access -- try ALIGN_KEYFRAME=0)
streamgate[1]: no playback after 40s (adb ok on 158/160 polls) -- nothing changed: baseline codec=none session=stopped, last poll codec=none session=stopped
```

`DEBUG=1` adds a line per poll showing both detection signals and the arming state.

---

## Troubleshooting

**Which build am I running?** `streamgate --version`. Release binaries are stamped with their tag; a build from source reports `dev`.

**Every tune times out.** Check that adb works from inside the container — `docker exec <container> adb devices` should list your tuner as `device`, not `offline` or `unauthorized`. Then check the box exposes at least one signal:

```sh
adb -s <ip>:5555 shell dumpsys media.resource_manager
adb -s <ip>:5555 shell "dumpsys media_session | grep -m1 PlaybackState"
```

**Tunes are slower than they used to be.** Four things add deliberate delay after playback is detected: `SETTLE` (0.25s), the motion gate (up to `MOTION_TIMEOUT`), keyframe alignment (up to `ALIGN_TIMEOUT` + 2s), and `WAIT_AUDIO` if you enabled it. The `aligned` log line breaks this down — `gate-to-air` is the total and `waited-for-motion` is the motion gate's share. If `gate-to-air` is small and the tune still felt slow, the time went somewhere downstream of this program.

**A flash at the head of the recording.** See above — raise `SETTLE`.

**Recording has sound but no picture.** Almost always upstream: confirm your encoder is actually carrying video. `ALIGN_KEYFRAME=0` will tell you quickly whether alignment is involved.

---

## Thank you

Thank you to [@sullrich](https://github.com/sullrich), [@bnhf](https://github.com/bnhf), and [@turtletank99](https://github.com/turtletank99) for the original `wait_for_video_playback_detection` idea in the excellent [ADBTuner](https://hub.docker.com/r/turtletank99/adbtuner).
