# ah4c-streamgatego

A gate for [ah4c](https://github.com/sullrich/ah4c) that waits until your Android box is **actually playing video** before handing the encoder stream to your DVR.

Without it, recording starts the moment the tuner is reserved — so the first several seconds of every tune are a splash screen, a loading spinner, or the channel you were on before. streamgate watches the box, waits for real playback, and only then opens the pipe.

The video is never re-encoded. The encoder's bytes go through untouched.

> The original POSIX shell implementation lives on the [`bash`](../../tree/bash) branch and still works. This branch is a single static binary that does the same job with a bundled curl doing the fetching, plus three things the shell script could not: start every tune with a live **cushion** of clean footage so playback doesn't buffer, start the output on a keyframe with the tables in front, and keep the channel-change tail out of the recording while doing both.

---

## What it will never do

Three properties hold on every code path. They are structural, not configuration:

- **stdout carries nothing but stream bytes.** The stream is fetched by the bundled curl and written to stdout untouched after the head trim; every log line — streamgate's and curl's — goes to stderr. There is no mode, flag or failure in which diagnostics can leak into the recording.
- **It never emits picture from before the gate opened.** Everything that arrives inside the render margin past the gate — including the encoder's buffered cache of the channel change, delivered as a burst at connect — is discarded, and output starts on the first keyframe after it. The one exception is the cached PAT/PMT injected at the head — program tables, not video. *(The ungated modes — no `TUNERn_IP`, or `ON_TIMEOUT=stream` — send whatever is on screen by definition, as they always did.)*
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

How the hook works: ah4c pipes `CMDn`'s stdout to your DVR — whatever the command writes *is* the stream, and a command that exits without writing fails the tune. streamgate writes nothing while it watches the box; once playback is real it has the bundled curl fetch the encoder and puts that stream on stdout, trimmed to start clean (see "How the stream is delivered" below). Two things follow from how ah4c runs the command: it is **exec'd directly, not run through a shell** (no `$VAR` expansion, no pipes — a binary plus a number is the clean form), and it **starts before your tune script fires**, which is deliberate: streamgate snapshots what the *previous* channel was doing so it can tell that apart from the new one.

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

H.264 in an MPEG-TS stream is mostly P and B frames, which are differences against earlier frames. Only an **IDR keyframe** stands alone: attach partway between keyframes and the decoder throws everything away until the next one. Encoders emit keyframes on a fixed cycle — every 2 seconds is typical — so a random attach point costs anywhere from zero to a full cycle, redrawn every tune. That is why the same box on the same channel can feel instant once and sluggish the next time.

streamgate therefore never starts the output mid-GOP: it starts on the first keyframe past the render margin, prefixed with the newest PAT and PMT seen, so your DVR decodes from the very first packet instead of waiting for the encoder's next keyframe and table cycle. If no keyframe is recognised, the hunt is bounded by `ALIGN_TIMEOUT` plus 2s of grace, after which it streams unaligned rather than produce no output at all.

## How the stream is delivered

The moment the gate opens, a statically linked curl 8.21.0 — compiled into
the binary, unpacked at tune time — connects to the encoder and fetches the
stream; streamgate shapes only the head of what it returns:

1. **The connect burst is discarded.** The encoder was never touched during
   the detection wait, so whatever its internal buffer held — footage of the
   channel change — arrives in the first instants and is dropped, along with
   everything else inside a 1-second render margin measured from the gate.
   The margin covers the lag between the box allocating its decoder (which is
   what detection sees) and real pixels reaching the screen, so the tuning
   card cannot reach the output. While this head-shaping runs, the pipe is
   kept warm with MPEG-TS null padding — clock, no picture — so your DVR's
   connection and probe overlap these waits instead of following them.
   Padding starts only after detection succeeded: a failing tune still puts
   zero bytes on the wire.

2. **Output starts on the first keyframe past the margin**, with the newest
   PAT/PMT in front so your DVR decodes from the very first packet. That is
   the live edge: the tune completes as fast as a clean start structurally
   can — the margin plus however long your encoder takes to mint the next
   keyframe, so a shorter encoder GOP means faster tunes.

3. **Then streamgate gets out of the way.** Everything after the head is a
   straight copy of curl's output — no parsing, no batching, no logic.

Starting at the live edge is a deliberate trade, stated plainly: **there is
no stall cushion.** Buffering protection, seconds behind live, and extra tune
seconds are the same number spent three ways, and this build spends none of
it — an encoder hiccup longer than your player's own small buffer will
surface as a brief stall. The cure for those is at the source: an encoder
that isn't wedged, CBR instead of VBR, a stable HDMI link. A tune with no
gate (`TUNERn_IP` unset) or a failed detection with `ON_TIMEOUT=stream`
skips the head shaping and streams as-is, which is what those modes have
always meant. `READ_TIMEOUT` maps onto curl's own low-speed abort — below
1000 bytes/s for that long ends the tune — so a wedged encoder cannot hold
the tuner slot forever.

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
| `ALIGN_TIMEOUT` | `8` | How long past the render margin to hunt for a keyframe (plus 2s of grace) before streaming unaligned rather than stalling. |
| `WAIT_AUDIO` | `0` | After the decoder appears, also wait for audio playback to start. Costs ~0.7s. Only needed if a flash survives `SETTLE` — it pins the exact moment the new channel renders, which the trim margin otherwise covers statistically. |
| `RENDER_TIMEOUT` | `3` | Cap on that wait, so a device that never reports audio still tunes. |
| `READ_TIMEOUT` | `10` | Give up when the encoder (nearly) stops sending: mapped onto curl's low-speed abort, below 1000 bytes/s for this long ends the tune. Catches a lost HDMI input or a wedged encode thread, which TCP keepalive does not — the peer is still answering. |
| `DEBUG` | unset | Log every poll. |

### If a recording starts on the app's tuning screen

First, check the `discarded … of pre-gate footage` figure — on most setups the burst trim handles this for you, and there is nothing to tune.

If yours doesn't, you have two options, in order:

**Raise `SETTLE`.** The decoder is allocated a little before the app puts real video on screen. `SETTLE` is a flat pause covering that gap; `0.5` or `0.75` is a reasonable next step. Cheap, but it's a timer — it doesn't know what's on screen, so it can be too short on a slow tune and wasted time on a fast one.

**Or set `WAIT_AUDIO=1`.** This waits for the app to start audio playback for the **new** tune before opening the gate — a *new* audio track id reaching the started state, compared against the tracks already playing at tune start, so the previous channel's still-running audio can't satisfy it. On these boxes it is the true render clock: video is tunneled and slaved to the audio hardware-sync output, so the first pixel cannot appear before that track runs. It costs the real time the box takes to begin playback — roughly 0.7s — and is bounded by `RENDER_TIMEOUT` so a device that never reports audio still tunes.

The comparison baseline is the first **whole** audio dump after detection, proven by an echoed marker the same way the detection probe proves its own dumps finished. A dump that arrives cut off cannot install a baseline that is silently missing the previous channel's track — that track would later read as *new* audio and open the gate on the card, which is the artifact this setting exists to prevent. A cut-off dump is retried a poll later; a transport that never delivers a whole dump falls back to accepting any started media track, which is never worse than not having the identity check at all.

---

## Logs

Every tune logs one detection line, the curl handoff line, and one alignment line:

```
streamgate[1]: playback detected after 5s via codec 1284494944 (base none), 1 confirmation(s)
streamgate[1]: fetching via bundled curl 8.21.0 (READ_TIMEOUT=10s maps to: abort below 1000 B/s for 10s)
streamgate[1]: aligned at the live edge 1.42s after the gate (1.0s render margin + keyframe wait), discarded 512KB of pre-gate cache; stall cushion is zero by design
```

What each number means:

| field | meaning |
|---|---|
| `playback detected after Ns` | time from the tuner being reserved until a decoder appeared. Mostly the box and the app; not much to do with this program. |
| `via codec … (base …)` | which signal fired. `base` is what was allocated before tuning — a *changed* id is the proof of new playback. |
| `aligned at the live edge Ns after the gate` | **the number to watch.** The whole cost of the clean start: the fixed render margin plus however long the encoder took to mint the next keyframe. A shorter encoder GOP shrinks the second part directly. |
| `discarded … of pre-gate cache` | the encoder's buffered footage of the channel change, delivered as a burst at connect and dropped inside the margin — the tail your recording must never open on. |

When detection can't confirm anything:

```
streamgate[1]: no playback after 40s (adb ok on 158/160 polls) -- nothing changed: baseline codec=none session=stopped, last poll codec=none session=stopped
```

### Other lines you may see

| line | what it means |
|---|---|
| `fetching via bundled curl …` | The gate opened and the bundled curl is fetching the encoder. curl runs with its errors visible, so a fetch problem prints curl's own message alongside streamgate's lines. |
| `encoder unavailable, no picture emitted yet (attempt N: …); redialling` | The encoder refused or died before any picture went out (null padding may already be flowing). Redials are bounded (3 attempts, capped by `READ_TIMEOUT`) so a dead encoder fails the tune instead of holding the DVR. |
| `no keyframe recognised within Ns of the margin; streaming unaligned` | No keyframe was found within `ALIGN_TIMEOUT`+2s past the render margin — the encoder may not signal random access. Streams anyway rather than produce no output. |
| `no 188-byte sync grid in this stream; accepting bare sync bytes` | The response body doesn't look like MPEG-TS. streamgate relaxes rather than emit nothing, but check what the encoder URL actually returns — an HTML error page is a common culprit. |
| `encoder sent no video (…)` | The encoder answered and then ended or died before anything decodable arrived. Exits 1; nothing was written. |
| `encoder ended the stream` | The encoder closed mid-recording. However politely it closed, that truncates the recording, so it is logged and exits 1. |
| `encoder fell below 1000 B/s for Ns (bundled curl exit 28)` | `READ_TIMEOUT`'s trigger: the encoder went (nearly) silent, curl aborted, and the tune ends. Exits 1. |
| `stream closed by the DVR` | The normal end of every recording — the DVR hung up. Exits 0. |
| `render confirmed after a further Nms` / `render not confirmed within Ns` | The `WAIT_AUDIO` gate: the new tune's audio track started, or the cap expired and the gate opened anyway. |
| `no whole audio dump in 3 attempts; render falls back to any started media track` | The `WAIT_AUDIO` baseline could not be verified whole; the gate degrades to the identity-blind check rather than waiting on a baseline it cannot trust. |
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

**Every tune times out.** The timeout message tells you which of five different things actually happened — it is written to be pasted into a support thread, so paste the whole line:

| the message says | what happened | what to do |
|---|---|---|
| `the whole budget went to connecting` | Connecting to the box ate the entire `TUNE_TIMEOUT`; it was never polled once. | Check `TUNERn_IP`, and that the box is reachable and awake; raise `TUNE_TIMEOUT` if it is genuinely slow to accept connections. |
| `every adb call to … failed or returned nothing` | The box never answered a probe. | Check `TUNERn_IP` includes `:5555`, and that the box authorises the ah4c container (see ah4c's own setup docs). |
| `never returned a dump this could read` | The box answered, but every dump arrived cut off or in a format streamgate doesn't recognise. | Likely a vendor build with a different reporting format — open an issue with the output of the command the message suggests. |
| `playback WAS seen but never held` | Detection fired but could not satisfy `CONFIRM` consecutive sightings past `MIN_WAIT` before time ran out. | Lower `CONFIRM` or `MIN_WAIT`, or raise `TUNE_TIMEOUT`. |
| `nothing changed: baseline codec=… session=…` | The box answered the whole time and simply never started playing anything new. | The problem is upstream of streamgate — the tune script, the app, or the channel. `DEBUG=1` shows both signals per poll. |

**Recordings start with the tail of the previous channel.** The gate isn't running. Look for `TUNERn_IP not set -- no gate` at tuner start — a missing or misspelled `TUNERn_IP` disables detection without failing anything.

**Tunes are slower than they used to be.** Three things add deliberate delay after playback is detected: `SETTLE` (0.25s), the 1-second render margin, and `WAIT_AUDIO` if you enabled it. Past those, the tune is waiting for your encoder's next keyframe — the `aligned at the live edge Ns after the gate` line shows the total, and a shorter encoder GOP shrinks it directly. If that number is small and the tune still felt slow, the time went to detection (see `playback detected after Ns`) or somewhere downstream of this program.

**A flash at the head of the recording.** See above — raise `SETTLE`, or set `WAIT_AUDIO=1`, which pins the exact render moment instead of relying on the margin.

**Recording has sound but no picture.** Almost always upstream: confirm your encoder is actually carrying video.

**Live TV buffers.** This build plays at the live edge with zero stall cushion, on purpose — buffering protection and latency behind live are the same seconds, and it spends none. Every upstream hiccup longer than your player's own small buffer is therefore visible, and the fix is at the source: reboot an encoder that has been up for months (wedged encoder sessions are real), prefer CBR over VBR so bitrate peaks can't outrun the link, and check the HDMI chain. If the hiccups can't be fixed at the source, the only remaining lever is trading latency back for cushion — which is a code change, deliberately not a setting.

**Recordings end early.** Look at the exit line. `encoder ended the stream` means the encoder closed the connection mid-recording; `encoder fell below 1000 B/s` means it went (nearly) silent while holding the connection open. Both are the encoder's doing — streamgate never ends a recording on its own.

---

## Thank you

Thank you to [@sullrich](https://github.com/sullrich), [@bnhf](https://github.com/bnhf), and [@turtletank99](https://github.com/turtletank99) for the original `wait_for_video_playback_detection` idea in the excellent [ADBTuner](https://hub.docker.com/r/turtletank99/adbtuner).
