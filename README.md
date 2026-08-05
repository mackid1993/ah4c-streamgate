# ah4c-streamgatego

A gate for [ah4c](https://github.com/sullrich/ah4c) that waits until your Android box is **actually playing video** before handing the encoder stream to your DVR.

Without it, recording starts the moment the tuner is reserved — so the first several seconds of every tune are a splash screen, a loading spinner, or the channel you were on before. streamgate watches the box, waits for real playback, and only then opens the pipe.

The video is never re-encoded. The encoder's bytes go through untouched.

The encoder is fetched with a bundled static `curl`, so the HTTP side is handled by the tool that has spent thirty years learning how encoders misbehave. Its output is read by streamgate rather than written straight to your DVR, which is what makes everything below possible.

> The original POSIX shell implementation lives on the [`bash`](../../tree/bash) branch and still works. This branch is a single static binary that does the same job plus three things a shell script structurally cannot: hold the encoder connection open during the wait, start the output on a keyframe, and guarantee that no picture received before the gate opened is ever emitted.

---

## What it will never do

Three properties hold on every code path. They are structural, not configuration:

- **stdout carries nothing but stream bytes.** Every log line goes to stderr. There is no mode, flag or failure in which diagnostics can leak into the recording.
- **It never emits picture from before the gate opened.** It only ever discards forward. The one exception is the cached PAT/PMT injected at the head — program tables, not video — so it is structurally incapable of introducing a channel-change banner, a tuning prompt or a transition frame that a plain byte-copy would not also have delivered.
- **It never re-encodes, remuxes or edits.** Once the first byte is out, everything passes through byte-exact.

---

## Install

Download a binary from [Releases](../../releases) — `linux-amd64`, `linux-arm64`, or `linux-armv7`. They're statically linked, so there's nothing to install alongside them.

```sh
curl -fsSL -o /path/to/ah4c/scripts/streamgate \
  https://github.com/mackid1993/ah4c-streamgatego/releases/latest/download/streamgate-linux-amd64
chmod +x /path/to/ah4c/scripts/streamgate
```

That directory is the one you already bind-mount for ah4c; inside the container it's `/opt/scripts`.

Every release also ships a `SHA256SUMS` manifest, so the download can be verified:

```sh
curl -fsSLO https://github.com/mackid1993/ah4c-streamgatego/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

---

## Setting up the tuners

streamgate is wired in per tuner with three environment variables, set on the ah4c container (it inherits ah4c's environment). For tuner `n`:

| Variable | Required | What it is |
|---|---|---|
| `CMDn` | yes | The command whose stdout becomes tuner *n*'s stream: the streamgate path plus the tuner number, nothing else. |
| `ENCODERn_URL` | yes | The HTTP URL of tuner *n*'s encoder stream. If it's missing, streamgate exits immediately with a configuration error. |
| `TUNERn_IP` | yes, in practice | The Android box's network address, **including the port** (`:5555`). A missing or misspelled `TUNERn_IP` is *not* fatal: that tuner logs `TUNERn_IP not set -- no gate` once and then streams ungated — so it records the head of the previous channel on every tune, indefinitely, with no other complaint. Check the spelling. |

How the hook works: ah4c pipes `CMDn`'s stdout to your DVR — whatever the command writes *is* the stream, and a command that exits without writing fails the tune. streamgate withholds stdout while it watches the box, then streams the encoder once playback is real. Two things follow from how ah4c runs the command: it is **exec'd directly, not run through a shell** (no `$VAR` expansion, no pipes — a binary plus a number is the clean form), and it **starts before your tune script fires**, which is deliberate: streamgate snapshots what the *previous* channel was doing so it can tell that apart from the new one.

### In an env file

```ini
CMD1=/opt/scripts/streamgate 1
TUNER1_IP=192.168.1.31:5555
ENCODER1_URL=http://192.168.1.41/live/stream0

CMD2=/opt/scripts/streamgate 2
TUNER2_IP=192.168.1.32:5555
ENCODER2_URL=http://192.168.1.42/live/stream0
```

Quoted or unquoted both work. Docker's `--env-file` passes quotes and stray whitespace through literally rather than stripping them, so `TUNER1_IP="192.168.1.31:5555"` reaches the program quotes-and-all — streamgate strips them itself, along with any optional setting written the same way.

### In docker-compose

Either point `env_file:` at the file above, or put the same variables under `environment:` on your existing ah4c service:

```yaml
services:
  ah4c:
    # ...your existing ah4c service...
    environment:
      - CMD1=/opt/scripts/streamgate 1
      - TUNER1_IP=192.168.1.31:5555
      - ENCODER1_URL=http://192.168.1.41/live/stream0
      - CMD2=/opt/scripts/streamgate 2
      - TUNER2_IP=192.168.1.32:5555
      - ENCODER2_URL=http://192.168.1.42/live/stream0
```

Restart the container after either change. That's the whole setup — every other variable below is optional, and the defaults are meant to be good.

---

## How it decides playback is real

Two signals, cheapest first, both from a single probe of the box per poll.

**1. A new secure video decoder.** The box's resource manager lists which processes hold a video codec. Android issues a new client id per playback session, so streamgate records the **set** of ids before tuning and waits for one that wasn't in it.

That distinction matters more than it looks. Asking whether a decoder merely *exists* returns true immediately, because the previous channel's decoder is often still allocated when the gate starts — an in-place channel switch tears nothing down. Comparing sets rather than picking one id also means it doesn't matter what order the device lists them in, which varies.

**2. The media session.** The box reporting a session in the playing state at normal speed. Used when the first signal isn't available — older Android, or vendor builds that report codecs differently.

This one has no identity, so it only counts once it has dropped at least once since the gate started. A channel that just ended can leave its session parked at exactly "playing", and without that rule it would read as instant success.

Whichever fires must hold for `CONFIRM` consecutive polls.

Both rest on the baseline being real, so the timing baseline is taken by the first poll that actually comes back, and the codec baseline by the first one whose dump arrived whole — not by one unchecked call at startup. An empty dump makes every decoder look new and reads as "not playing", so a single failed probe would otherwise open the gate immediately on the channel you're leaving. A dump is only believed whole once it reaches its terminator, because "there was nothing to list" and "the list was cut off" are identical to a substring test. If the connection to the box drops mid-wait, streamgate reconnects rather than spending the rest of `TUNE_TIMEOUT` failing.

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

Motion seen *before* the gate — the wake animation, the home screen, the app launching, the channel you're leaving — is never evidence about the new channel, so the latch is always dropped when the gate opens and motion must prove itself again on the new picture. The learned floor is kept, so a warm tune re-latches within `MOTION_HOLD` windows (~750ms at defaults, usually inside the keyframe wait it already pays) while the card, sitting at the floor, cannot. `REARM_MOTION=1` additionally forgets the floor itself — for apps whose pre-gate picture is so quiet that a kept floor would let the card read as a rise. Off by default; the same trade-off note below applies.

## If the encoder goes quiet

**streamgate passes data through.** It does not buffer, pad, pace, or manufacture packets. What the encoder sends is what your DVR gets, and when the encoder sends nothing, nothing is what goes out. Holding video back to smooth it over would put streamgate between you and live TV, which is the one thing it is built not to do.

What it does instead is *measure*. Any silence longer than 150ms is reported once, when it ends:

```
streamgate[1]: gap in the encoder's output: 1.24s
```

That line exists to settle a question, not to fix anything. If recordings stutter **and** you see these lines, the encoder is dropping out and the fault is upstream of streamgate. If recordings stutter and you never see one, then the encoder never stopped sending and the cause is elsewhere entirely.

Silence is still bounded: once it passes `READ_TIMEOUT`, the tune fails loudly rather than holding your tuner open on a stream that is not coming back.

---

## Optional settings

Everything here is optional, set the same way as the tuner variables — in the env file or under `environment:` in compose. Durations accept either seconds (`5`, `0.25`) or Go syntax (`10s`, `250ms`). A value that can't be parsed is ignored with a warning on stderr rather than silently falling back.

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
| `ON_TIMEOUT` | `fail` | `fail` exits without streaming, so your DVR sees a dead tune and can retry or pick another tuner. `stream` sends whatever is on screen instead; `continue` is accepted as an alias of `stream`. An unrecognised value warns and keeps `fail` — a typo here used to disable the fail-safe silently. |
| `ALIGN_KEYFRAME` | `1` | Start output on a keyframe. `0` streams from wherever the encoder happens to be — and because the motion gate runs while waiting to align, `0` turns `WAIT_MOTION` off too. |
| `ALIGN_TIMEOUT` | `8` | If no keyframe is recognised within this long (plus 2s of grace), stream unaligned rather than stall. Automatically raised above `MOTION_TIMEOUT` if you set them so they'd conflict. |
| `WAIT_AUDIO` | `0` | After the decoder appears, also wait for audio playback to start. Costs ~0.7s. Only needed if a flash survives `SETTLE`. |
| `RENDER_TIMEOUT` | `3` | Cap on that wait, so a device that never reports audio still tunes. |
| `WAIT_MOTION` | `1` | Wait for the picture to start moving before releasing. `0` releases on the first keyframe. |
| `MOTION_WINDOW` | `0.25` | Seconds per measurement window. |
| `DRAIN_IDLE` | `500us` | At handoff, discard video the encoder sent while we were still waiting on the box, so playback starts from live rather than from whatever had queued up. A read faster than this came from a buffer, not the network. `0` disables it. **Note the unit:** this is the only setting here measured in microseconds, and a bare number is read as seconds — `DRAIN_IDLE=1` means one second, which would discard the head of every recording. Catching up is also capped at 2s regardless. |
| `MOTION_HOLD` | `3` | Consecutive windows above the threshold before it counts as motion. Filters out brief spikes from the cut itself. Values below `2` are raised to `2` with a warning — a single window straddling the gate (old picture on one side, card on the other) could otherwise read as motion. |
| `RISE_FACTOR` | `5` | How far above the quietest observed window a window must rise. A ratio, not a bitrate. |
| `MOTION_TIMEOUT` | `6` | Give up waiting for motion after this long and release anyway. |
| `REARM_MOTION` | `0` | The motion *latch* is always dropped at the gate; this additionally forgets the learned *floor* and re-learns it from the new channel. Only needed if a splash frame survives the default behaviour, at the cost of `MOTION_TIMEOUT` on switches that never show a card. |
| `READ_TIMEOUT` | `10` | Give up if the encoder holds the connection open but sends nothing for this long. Catches a lost HDMI input or a wedged encode thread, which TCP keepalive does not — the peer is still answering. Every successful read pushes it out, so it only fires on genuine silence. |
| `DEBUG` | unset | Log every poll. |

### If a recording starts on the app's tuning screen

First, see the section above — on most encoders the scene-change keyframe handles this for you, and there is nothing to tune.

If yours doesn't, you have two options, in order:

**Raise `SETTLE`.** The decoder is allocated a little before the app puts real video on screen. `SETTLE` is a flat pause covering that gap; `0.5` or `0.75` is a reasonable next step. Cheap, but it's a timer — it doesn't know what's on screen, so it can be too short on a slow tune and wasted time on a fast one.

**Or set `WAIT_AUDIO=1`.** This waits for the app to start audio playback for the **new** tune before opening the gate — a *new* audio track id reaching the started state, compared against the tracks already playing at tune start, so the previous channel's still-running audio can't satisfy it. On these boxes it is the true render clock: video is tunneled and slaved to the audio hardware-sync output, so the first pixel cannot appear before that track runs. It costs the real time the box takes to begin playback — roughly 0.7s — and is bounded by `RENDER_TIMEOUT` so a device that never reports audio still tunes.

The comparison baseline is the first **whole** audio dump after detection, proven by an echoed marker the same way the detection probe proves its own dumps finished. A dump that arrives cut off cannot install a baseline that is silently missing the previous channel's track — that track would later read as *new* audio and open the gate on the card, which is the artifact this setting exists to prevent. A cut-off dump is retried a poll later; a transport that never delivers a whole dump falls back to accepting any started media track, which is never worse than not having the identity check at all.

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

### Other lines you may see

| line | what it means |
|---|---|
| `encoder unavailable, nothing emitted yet (attempt N: …); redialling` | The encoder connection failed before a single byte went out, so retrying is free — nothing is lost while the gate is shut. Once the gate is open, redials are bounded (3 attempts, capped by `READ_TIMEOUT`) so a dead encoder fails the tune instead of holding the DVR. |
| `no 188-byte sync grid in this stream; accepting bare sync bytes` | The response body doesn't look like MPEG-TS. streamgate relaxes rather than emit nothing, but check what the encoder URL actually returns — an HTML error page is a common culprit. |
| `no picture on the PMT-declared video pids for Ns; accepting any non-padding packet` | The stream's own PMT names a video PID the mux never carries. Observation outranks the table: streamgate stops trusting the PMT once, then fails only if there is still no picture. |
| `encoder sent no picture for Ns, only padding and tables` | The encoder is muxing with nothing on its input — null padding and SI tables, no video. Exits 1 rather than shipping a dead mux to the DVR for the length of a programme. Check the HDMI input. |
| `encoder sent no video (…)` | The encoder answered 200 and then ended or died before a single video byte — a genuine silence. Exits 1; nothing was written. |
| `encoder refused the request (bundled curl exit 22)` | The encoder answered with an HTTP error. The line directly above is curl's, and it names the status — a `503` here usually means every session on that encoder is already in use. |
| `cannot reach the encoder (bundled curl exit 7)` | Nothing answered at `ENCODERn_URL`. Check the address and that the encoder is powered up. |
| `encoder timed out (bundled curl exit 28)` | Either the connection took more than 5s to establish, or the transfer stalled. curl's line above says which. |
| `bundled curl exited N` | Any other curl failure. Its own message sits directly above with the detail. |
| `encoder ended the stream after X and Y MB` | The encoder closed mid-recording. However politely it closed, that truncates the recording, so it is logged and exits 1. |
| `gap in the encoder's output: Ns` | The encoder stopped sending for longer than 150ms, then resumed. One line per gap. Nothing was done about it — see "If the encoder goes quiet". |
| `encoder sent nothing for Ns` | The silence outran `READ_TIMEOUT`, so the tune failed rather than holding the tuner on a stream that was not coming back. |
| `stream closed by the DVR` | The normal end of every recording — the DVR hung up. Exits 0. |
| `render confirmed after a further Nms` / `render not confirmed within Ns` | The `WAIT_AUDIO` gate: the new tune's audio track started, or the cap expired and the gate opened anyway. |
| `no whole audio dump in 3 attempts; render falls back to any started media track` | The `WAIT_AUDIO` baseline could not be verified whole; the gate degrades to the identity-blind check rather than waiting on a baseline it cannot trust. |
| `raising MOTION_HOLD=N to 2 …` | A configured value below the structural floor was clamped, not defaulted. |
| `ignoring X="…" -- …; using the default` | A setting failed to parse. The line says exactly which value and why. |

`DEBUG=1` adds a line per poll showing both detection signals and the arming state:

```
streamgate[1]: t=1.2s codec=abc123 base=abc123 session=stopped armed=true hits=0 playing=false
```

`codec`/`base` are the current and baseline decoder id sets — detection fires when an id appears that the baseline lacks. `session` is the media-session fallback's reading, `armed` is whether that fallback has earned trust by dropping at least once, and `hits` counts consecutive confirmations toward `CONFIRM`.

---

## Exit status

| code | meaning |
|---|---|
| `0` | The recording ended the way recordings end: the DVR closed the pipe. Also `--version`. |
| `1` | The tune failed, and the last log line says why — detection timed out with `ON_TIMEOUT=fail`, the encoder died or carried no picture, or the stream ended mid-recording. ah4c surfaces this as a failed tune, which your DVR can retry. |
| `2` | Configuration: no tuner-number argument, or `ENCODERn_URL` unset. Nothing was attempted. |

---

## Troubleshooting

**Which build am I running?** `streamgate --version`. Release binaries are stamped with their tag; a build from source reports `dev`.

**There are `curl: (NN) …` lines in my logs.** Those are the bundled curl's own, and they are meant to be there — curl fetches the encoder, so when the fetch fails it reports the reason first and streamgate names the fault on the line below it. Paste both lines; between them they say exactly what happened.

**Every tune times out.** The timeout message tells you which of five different things actually happened — it is written to be pasted into a support thread, so paste the whole line:

| the message says | what happened | what to do |
|---|---|---|
| `the whole budget went to connecting` | Connecting to the box ate the entire `TUNE_TIMEOUT`; it was never polled once. | Check `TUNERn_IP`, and that the box is reachable and awake; raise `TUNE_TIMEOUT` if it is genuinely slow to accept connections. |
| `every adb call to … failed or returned nothing` | The box never answered a probe. | Check `TUNERn_IP` includes `:5555`, and that the box authorises the ah4c container (see ah4c's own setup docs). |
| `never returned a dump this could read` | The box answered, but every dump arrived cut off or in a format streamgate doesn't recognise. | Likely a vendor build with a different reporting format — open an issue with the output of the command the message suggests. |
| `playback WAS seen but never held` | Detection fired but could not satisfy `CONFIRM` consecutive sightings past `MIN_WAIT` before time ran out. | Lower `CONFIRM` or `MIN_WAIT`, or raise `TUNE_TIMEOUT`. |
| `nothing changed: baseline codec=… session=…` | The box answered the whole time and simply never started playing anything new. | The problem is upstream of streamgate — the tune script, the app, or the channel. `DEBUG=1` shows both signals per poll. |

**Recordings start with the tail of the previous channel.** The gate isn't running. Look for `TUNERn_IP not set -- no gate` at tuner start — a missing or misspelled `TUNERn_IP` disables detection without failing anything.

**Tunes are slower than they used to be.** Four things add deliberate delay after playback is detected: `SETTLE` (0.25s), the motion gate (up to `MOTION_TIMEOUT`), keyframe alignment (up to `ALIGN_TIMEOUT` + 2s), and `WAIT_AUDIO` if you enabled it. The `aligned` log line breaks this down — `gate-to-air` is the total and `waited-for-motion` is the motion gate's share. If `gate-to-air` is small and the tune still felt slow, the time went somewhere downstream of this program.

**A flash at the head of the recording.** See above — raise `SETTLE`, or set `WAIT_AUDIO=1`.

**Recording has sound but no picture.** Almost always upstream: confirm your encoder is actually carrying video. `ALIGN_KEYFRAME=0` will tell you quickly whether alignment is involved.

**Recordings end early.** Look at the exit line. `encoder ended the stream after …` means the encoder closed the connection mid-recording; `READ_TIMEOUT` firing means it went silent while holding the connection open. Both are the encoder's doing — streamgate never ends a recording on its own.

---

## Thank you

Thank you to [@sullrich](https://github.com/sullrich), [@bnhf](https://github.com/bnhf), and [@turtletank99](https://github.com/turtletank99) for the original `wait_for_video_playback_detection` idea in the excellent [ADBTuner](https://hub.docker.com/r/turtletank99/adbtuner).
