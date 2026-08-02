# ah4c-streamgate

A gate for [ah4c](https://github.com/sullrich/ah4c) that waits until your Android box is **actually playing video** before handing the encoder stream to Channels DVR.

Without it, Channels starts recording the moment the tuner is reserved — so the first several seconds of every tune are the app's splash screen, a loading spinner, or the previous channel. streamgate watches the box over adb, waits for real playback, and only then opens the pipe.

The stream itself is untouched. No re-encode, no remux — the encoder's bytes go straight through.

---

## Requirements

- ah4c (`bnhf/ah4c` or equivalent) with a bind-mounted scripts directory
- An HDMI encoder reachable over HTTP (LinkPi, Osprey, etc.)
- An Android device with adb over TCP enabled
- `adb` and `curl` inside the container — both ship in `bnhf/ah4c`

---

## Install

Drop the script in the scripts directory you already bind-mount for ah4c — whatever `${HOST_DIR}/ah4c/scripts` points at on your host. Inside the container that path is `/opt/scripts`.

```sh
cp streamgate.sh /path/to/ah4c/scripts/
chmod +x /path/to/ah4c/scripts/streamgate.sh
```

Then point each tuner's `CMD` at it, with the tuner number as the only argument:

```
CMD1=/opt/scripts/streamgate.sh 1
CMD2=/opt/scripts/streamgate.sh 2
```

Restart the container. That's the whole install.

---

## How CMDn works in ah4c

ah4c has two ways to source a tuner's video.

**Without `CMDn`** it fetches `ENCODERn_URL` itself and copies those bytes to Channels.

**With `CMDn`** it runs your command instead and pipes **that command's stdout** to Channels. Whatever the command writes is the stream. If the command exits without writing anything, the tune fails.

That's the hook streamgate uses: it withholds stdout while it watches the box, then `exec`s curl against the encoder once playback is real.

Two details worth knowing, because they shape how the `.env` line has to look:

**ah4c does not run `CMDn` through a shell.** It splits the string on spaces (single quotes group, and are stripped) and execs the result directly. So there is no `$VAR` expansion, no pipes, no `&&`. A script plus a numeric argument is the clean form — which is why streamgate takes a tuner number and reads everything else from the environment.

**`CMDn` runs concurrently with your tune script.** ah4c starts the command, then fires `bmitune.sh` when Channels first reads. streamgate is already polling before the deeplink lands, which is deliberate — it snapshots what the *previous* channel was doing so it can tell that apart from the new one.

### docker-compose.yml

Nothing special is required beyond what ah4c already needs. The two relevant pieces:

```yaml
services:
  ah4c:
    image: bnhf/ah4c:${TAG}
    environment:
      - TUNER1_IP=${TUNER1_IP}
      - ENCODER1_URL=${ENCODER1_URL}
      - CMD1=${CMD1}
      # ...one set per tuner
    volumes:
      - ${HOST_DIR}/ah4c/scripts:/opt/scripts
```

The `environment:` block is how `TUNERn_IP` and `ENCODERn_URL` reach the script — it reads them from its own environment rather than taking them as arguments. The `volumes:` line is where you put the script.

### .env

```
##Tuner 1
TUNER1_IP=192.168.1.16:5555
ENCODER1_URL=http://192.168.1.90:8090/stream0
CMD1=/opt/scripts/streamgate.sh 1
##Tuner 2
TUNER2_IP=192.168.1.17:5555
ENCODER2_URL=http://192.168.1.90:8090/stream1
CMD2=/opt/scripts/streamgate.sh 2
```

`TUNERn_IP` must include the adb port, normally `:5555`.

> **Note:** setting `CMDn` moves that tuner off ah4c's network-encoder path, which is also where `NULL_FRAME_INSERTION` lives. That feature does not apply to a tuner using `CMDn`.

---

## Knobs

Set these in the compose `environment:` block. They apply to every tuner, since they all share the container's environment.

| Variable | Default | What it does |
|---|---|---|
| `MIN_WAIT` | `1` | Ignore "playing" for this many seconds after start. Guards against latching onto the channel that was already up. |
| `TUNE_TIMEOUT` | `40` | Give up after this many seconds. **Costs nothing on a tune that works** — the loop returns the moment it detects. It only governs how long a slow or cold box gets. |
| `CONFIRM` | `3` | Consecutive polls that must agree before handing off. This is the anti-flash knob — see below. |
| `POLL` | `0.25` | Seconds between polls. |
| `SETTLE` | `0.25` | Pause after detecting, before opening the pipe. |
| `ON_TIMEOUT` | `fail` | `fail` exits without streaming, so Channels sees a dead tune and can retry or move on. `stream` sends the encoder anyway — you'll record whatever is on screen. |
| `BLACK_CHECK` | `auto` | HDCP-black fallback. `auto` holds it back until `BLACK_AFTER`, `1` samples from the start, `0` disables it. |
| `BLACK_AFTER` | `6` | In `auto`, how long the cheap checks get before the screengrab starts. |
| `BLACK_PCT` | `100` | Percent black required. `pblack` is a truncated integer, so `100` means fully black. |
| `DEBUG` | unset | `1` logs every poll. |

### Which ones you'd actually change

**`CONFIRM`** if you still see a flash of splash screen. A deeplink into an already-running app switches channels in place — the log line `Activity not started, intent has been delivered to currently running top-most instance` — and the new decoder is allocated a beat before anything is decoded. One sighting isn't playback. Raise to 4 or 5 if a flash gets through; each step costs `POLL` seconds.

**`TUNE_TIMEOUT`** if boxes that have been asleep a while take longer than 40s to come up, or lower it if you'd rather Channels find out quickly that a tuner is dead.

**`ON_TIMEOUT`** depending on what you prefer when detection never fires: a failed tune your DVR can retry (`fail`), or a recording of a splash screen (`stream`).

**`BLACK_CHECK=1`** only if your device populates neither of the primary signals — see [Troubleshooting](#troubleshooting). It is slower and less precise, and it will claim playback on a box whose screen is simply off.

---

## How detection works

Every poll is a single adb round trip that pulls two `dumpsys` outputs at once. Three signals, cheapest first.

**1. Secure decoder identity.** `dumpsys media.resource_manager` lists which processes hold a secure video codec. Android issues a new client id per playback session, so the script snapshots the id before tuning and waits for a *different* one.

This distinction matters more than it looks. Checking whether a decoder merely *exists* returns true immediately, because the previous channel's decoder is still allocated when the gate starts — nothing is torn down on an in-place channel switch.

**2. Media session.** `dumpsys media_session` reporting `state=3` with `speed=1`. A channel that ended can leave its session parked at exactly that with the position frozen, so this one only counts once it has dropped at least once since the gate started.

**3. HDCP black screen.** While protected content plays, HDCP blanks the framebuffer, so a screengrab comes back fully black. Real signal, but expensive — capture plus decode is about a second — and blind in one direction, since a screen that's simply off is just as black. That's why it's last and starts late.

Whichever fires must hold for `CONFIRM` consecutive polls. A disagreeing poll resets the count to zero.

---

## Logs

Every tune logs one line saying which signal decided it:

```
streamgate[1]: playback detected after 6s via codec 1543877488 (was 1450196752), 3 confirmations
streamgate[1]: playback detected after 4s via session playing, 3 confirmations
streamgate[1]: playback detected after 9s via hdcp black screen, 3 confirmations
```

And on failure:

```
streamgate[1]: no playback after 40s (codec=none base=none session=unknown)
streamgate[1]: failing the tune rather than streaming whatever is on screen
```

`DEBUG=1` adds a line per poll:

```
streamgate[1]: baseline codec=1450196752 session=playing
streamgate[1]: t=25ds codec=1450196752 base=1450196752 session=playing armed=0 hits=0 -> playing
streamgate[1]: t=61ds codec=1543877488 base=1450196752 session=playing armed=1 hits=1 -> playing
```

---

## Troubleshooting

**Every tune says `via session`, never `via codec`.** Your device isn't exposing decoder allocations. Check it directly:

```sh
adb -s <ip>:5555 shell dumpsys media.resource_manager
```

During playback you want `Id:` and `secure-codec` lines beneath a `Pid:` inside the `Processes:` block. If they only appear under `Events logs`, or not at all, detection is resting entirely on the media session.

**Every tune times out.** Check whether either signal ever appears:

```sh
adb -s <ip>:5555 shell dumpsys media_session | grep -m1 PlaybackState
```

`state=3` with `speed=1` during playback means signal 2 works. If neither signal shows up on your hardware, `BLACK_CHECK=1` is the fallback.

**Still seeing a flash of splash screen.** Raise `CONFIRM`. The gap between "decoder allocated" and "frames on HDMI" is device-dependent, and on some boxes three polls isn't enough to cover it.

**Tunes feel slower than the encoder looks.** Expected on a cold box. The signals the script watches trail what you see on the encoder when an app is starting from scratch, and a booting box answers adb more slowly, so polls stretch. On a warm in-place switch they land together.

**Detection works but the tune fails anyway.** Confirm `adb connect` succeeds from inside the container — `docker exec <container> adb devices` should list your tuner as `device`, not `offline` or `unauthorized`.

---

## Notes

The script assumes one video app per box, which is normal for a dedicated tuner. It doesn't scope the decoder check to a specific package — deriving the foreground app during a tune proved unstable, and the identity comparison covers it. On a box running several video apps simultaneously, another app starting protected playback mid-tune could in principle satisfy the check.

It also has no handling for profile prompts — "who's watching", "choose an account". If your app shows one, the gate will wait it out and time out. Handle those in your `bmitune.sh`.

The `secureCodecId` parser depends on the current `dumpsys media.resource_manager` layout. Android or firmware updates can reshape that output. After a box update, run one tune with `DEBUG=1` and confirm detections still say `via codec` — if they all fall back to `via session`, the parser needs re-fitting.

## Thank you

Thank you to @sullrich, @bnhf, and @turtletank99 for the original idea in the excellent ADBTuner.
