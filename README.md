# ah4c-streamgate

A gate for [ah4c](https://github.com/sullrich/ah4c) that waits until your Android box is **actually playing video** before handing the encoder stream to Channels DVR.

Without it, Channels starts recording the moment the tuner is reserved — so the first several seconds of every tune are the app's splash screen, a loading spinner, or the previous channel. streamgate watches the box over adb, waits for real playback, and only then opens the pipe.

The stream itself is untouched. No re-encode, no remux — the encoder's bytes go straight through.

> The original POSIX shell implementation lives on the [`bash`](../../tree/bash) branch and is still supported. This branch is a single static binary that does the same job, plus the two things a shell script structurally cannot: hold the encoder connection open during the wait, and start the output on a keyframe.

---

## Install

Grab a binary from [Releases](../../releases) — `linux-amd64`, `linux-arm64`, or `linux-armv7`. They're static (`CGO_ENABLED=0`), so they run in the ah4c container as-is.

Drop it in the scripts directory you already bind-mount for ah4c. Inside the container that path is `/opt/scripts`.

```sh
cp streamgate-linux-amd64 /path/to/ah4c/scripts/streamgate
chmod +x /path/to/ah4c/scripts/streamgate
```

Then point each tuner's `CMD` at it, with the tuner number as the only argument:

```
CMD1=/opt/scripts/streamgate 1
CMD2=/opt/scripts/streamgate 2
```

Restart the container. `TUNERn_IP` and `ENCODERn_URL` are read from the environment — nothing else to configure.

---

## How CMDn works in ah4c

ah4c has two ways to source a tuner's video.

**Without `CMDn`** it fetches `ENCODERn_URL` itself and copies those bytes to Channels.

**With `CMDn`** it runs your command instead and pipes **that command's stdout** to Channels. Whatever the command writes is the stream. If the command exits without writing anything, the tune fails.

That's the hook streamgate uses: it withholds stdout while it watches the box, then streams the encoder once playback is real.

Two details worth knowing:

**ah4c does not run `CMDn` through a shell.** It splits the string on spaces and execs the result directly — no `$VAR` expansion, no pipes. A binary plus a numeric argument is the clean form.

**`CMDn` runs concurrently with your tune script.** ah4c starts the command, then fires `bmitune.sh` when Channels first reads. streamgate is already polling before the deeplink lands, which is deliberate — it snapshots what the *previous* channel was doing so it can tell that apart from the new one.

---

## Knobs

All optional, all read from the environment.

| Variable | Default | What it does |
|---|---|---|
| `CONFIRM` | `1` | Consecutive sightings required before handing off. Raise to `2`+ if a splash frame ever gets through. |
| `POLL` | `0.25` | Seconds between polls while still waiting for a first sighting. |
| `CONFIRM_POLL` | `0.05` | Seconds between polls once something has been sighted. Confirming is the one phase on the critical path, so it polls tight. |
| `MIN_WAIT` | `1` | Ignore "playing" for this many seconds after start. |
| `SETTLE` | `0` | Pause after detecting, before handing off. |
| `TUNE_TIMEOUT` | `40` | Give up after this many seconds. Costs nothing on a tune that works. |
| `ON_TIMEOUT` | `fail` | `fail` exits without streaming so Channels can retry or move on; anything else streams whatever is on screen. |
| `ALIGN_KEYFRAME` | `1` | Start output on a keyframe. `0` streams from wherever the encoder happens to be. |
| `DEBUG` | unset | Log every poll. |

---

## How detection works

`dumpsys media.resource_manager` lists which processes hold a secure video codec. Android issues a new client id per playback session, so streamgate snapshots the id before tuning and waits for a **different** one.

This distinction matters more than it looks. Checking whether a decoder merely *exists* returns true immediately, because the previous channel's decoder may still be allocated when the gate starts — nothing is torn down on an in-place channel switch.

Decoder allocation is the earliest signal the box will give you. Everything that indicates actual rendering — audio start, media session state, framebuffer contents — lands measurably later, and on a DRM/tunneled pipeline most of them never populate at all: `SurfaceFlinger --latency` returns zeros for the video layer, `screencap` is HDCP-black, and `PlaybackState.position` stays at zero.

---

## Keyframe alignment

The gate opening and the DVR having a *picture* are not the same instant.

H.264 in an MPEG-TS stream is mostly P/B frames, which are diffs against earlier frames. Only an IDR keyframe is self-contained. Attach mid-GOP and the decoder must discard everything until the next one — a wait of anywhere from zero to your encoder's full GOP length, redrawn at random on every tune. On a 2-second GOP that is a 0–2s coin flip; it is the single largest source of tune-to-tune inconsistency in this pipeline.

So when the gate opens, streamgate discards forward to the next keyframe on a video PID and emits from there, prefixed with the most recently seen PAT and PMT so the DVR can parse the stream immediately rather than waiting for the encoder's next PSI cycle.

**It only ever discards forward.** It never emits a byte received before the gate opened, so it cannot introduce a channel-change banner, a tuning prompt, or a transition frame that a plain byte-copy would not also have delivered.

---

## Logs

Every tune logs one line:

```
streamgate[1]: playback detected after 5s via codec 1284494944 (was none), 1 confirmation(s)
streamgate[1]: aligned to keyframe on pid 256 after discarding 412 packets (75 KB)
```

And on failure:

```
streamgate[1]: no playback after 40s (base=none)
streamgate[1]: failing the tune rather than streaming whatever is on screen
```

---

## Building

CI builds every push to `main` and publishes binaries on any `v*` tag. You can also run it by hand from the Actions tab — leave `release_tag` empty to just get artifacts, or set it to cut a release.

Locally:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o streamgate .
```

---

## Thank you

Thank you to [@sullrich](https://github.com/sullrich), [@bnhf](https://github.com/bnhf), and [@turtletank99](https://github.com/turtletank99) for the original `wait_for_video_playback_detection` idea in the excellent [ADBTuner](https://hub.docker.com/r/turtletank99/adbtuner).
