#!/bin/sh
#
# streamgate.sh -- ah4c CMDn helper. Waits for the Android box to actually be
# playing video, then streams the encoder through untouched.
#
# Put it in the scripts directory you bind-mount for ah4c (/opt/scripts inside
# the container), make it executable, then in the .env:
#
#   CMD1=/opt/scripts/streamgate.sh 1
#
# TUNERn_IP and ENCODERn_URL are read from the environment.
# Self-contained: nothing needs adding to the pre/stop scripts.
#
# DEBUG=1 logs every poll.

MIN_WAIT=${MIN_WAIT:-1}          # ignore "playing" before this many seconds
TUNE_TIMEOUT=${TUNE_TIMEOUT:-40} # give up and stream anyway after this. Costs
                                 # nothing on a tune that works -- the loop
                                 # returns the moment it detects -- so this only
                                 # governs how long a cold box gets before you
                                 # end up watching its splash screen.
SETTLE=${SETTLE:-0}           # pause after detecting, before handing off
CONFIRM=${CONFIRM:-1}            # consecutive polls that must agree before handing
                                 # off. A deeplink into an already-running app
                                 # switches channels in place -- "Activity not
                                 # started, intent delivered to currently running
                                 # top-most instance" -- so the new decoder is
                                 # allocated a beat before anything is decoded.
                                 # One sighting is not playback.
POLL=${POLL:-0.05}
CONFIRM_POLL=${CONFIRM_POLL:-0.05}
BLACK_CHECK=${BLACK_CHECK:-auto} # auto | 1 | 0 -- see the note below
BLACK_AFTER=${BLACK_AFTER:-6}    # in auto, only start sampling after this many seconds
BLACK_PCT=${BLACK_PCT:-100}      # pblack is an integer; 100 is the >99.8 rule
ON_TIMEOUT=${ON_TIMEOUT:-fail}   # fail | stream. On fail the script exits without
                                 # sending anything, so ah4c sees the tune die and
                                 # Channels can drop the tuner or move to another
                                 # one -- better than handing it a splash screen.

n="$1"
[ -n "$n" ] || { echo "usage: $0 <tuner-number>" >&2; exit 2; }
eval "TUNER_IP=\$TUNER${n}_IP"
eval "ENCODER_URL=\$ENCODER${n}_URL"
[ -n "$ENCODER_URL" ] || { echo "ENCODER${n}_URL not set" >&2; exit 2; }

log() { echo "streamgate[$n]: $*" >&2; }
adbsh() { adb -s "$TUNER_IP" shell "$@" 2>/dev/null | tr -d '\r'; }
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }

# One round trip per poll. Emits the resource_manager dump, then a marker, then
# the first PlaybackState line. No window/focus lookup: which app is in front
# is unstable exactly during a tune, and nothing below needs to know.
pollDevice() {
    adbsh 'dumpsys media.resource_manager; echo __MS__; dumpsys media_session | grep -m1 PlaybackState'
}

# The client id of whichever secure video decoder is allocated, or nothing.
# Android hands out a new client per playback session, so a *changed* id is the
# signal -- "a decoder exists" is not, since the previous channel's decoder is
# still allocated when this script starts.
secureCodecId() {
    printf '%s\n' "$1" | awk '
        /^[[:space:]]*Process Pid override/ { exit }
        /^[[:space:]]*Events logs/          { exit }
        /^[[:space:]]*Processes:/           { inproc = 1; next }
        !inproc { next }
        /^[[:space:]]*Id:/ { id = $2; next }
        /video-codec/ || /videoCodec/ {
            l = tolower($0)
            if (index(l, "non-secure") == 0 && index(l, "secure") > 0) {
                print id; found = 1; exit
            }
        }
        END { exit found != 1 }'
}

# state=2 stopped, state=3 (or PLAYING(3)) with speed=1 playing. Tested in this
# order because a line can carry both and playing is the stronger claim.
mediaSessionState() {
    case "$1" in
        *state=3*|*"PLAYING(3)"*)
            case "$1" in *speed=1*) echo playing; return ;; esac ;;
    esac
    case "$1" in
        *state=2*) echo stopped; return ;;
    esac
    echo unknown
}

# HDCP-black fallback -- a last resort, not a third opinion.
#
# When protected content plays, HDCP blanks the framebuffer, so a screengrab
# comes back all black. Real signal, expensive to read: screencap plus decode is
# about a second, and when the screen IS black it runs twice with a 0.3s gap to
# reject a single dark transition frame, so ~2.3s. The poll loop blocks for the
# whole of it, which stretches polls from ~0.5s apart to 2-3.5s apart and adds
# that much latency to detection.
#
# So it does not run on every poll. BLACK_CHECK=auto (the default) holds it back
# until BLACK_AFTER seconds have passed with the cheap checks finding nothing --
# a tune that resolves normally never pays for it at all, and a box that comes
# up cold, where neither a decoder nor a media session ever appears, still gets
# a signal instead of running out the clock and streaming the splash screen.
# Set 1 to sample from the start, 0 to disable it outright.
#
# It is blind in one direction: a screen that is merely off is just as black as
# one HDCP has blanked, so on a sleeping box it claims playback that is not
# happening. That is the reason it goes last and starts late.
#
# The range conversion is load-bearing. screencap returns full-range RGB and
# blackframe wants YUV, so ffmpeg's implicit scaler puts pure black at luma 16
# and a threshold of 1 would then match nothing at all.
screenIsBlack() {
    _pct=$(adb -s "$TUNER_IP" exec-out screencap -p 2>/dev/null |
           ffmpeg -hide_banner -nostats -v info -f png_pipe -i - \
                  -vf scale=in_range=full:out_range=full,format=gray,blackframe=amount=0:threshold=1 \
                  -f null - 2>&1 |
           sed -n 's/.*pblack:\([0-9]*\).*/\1/p' | head -1)
    [ -n "$_pct" ] || return 1
    [ "$_pct" -ge "$BLACK_PCT" ]
}
blackScreenPlaying() {
    screenIsBlack || return 1
    sleep 0.3
    screenIsBlack
}

splitRM() { printf '%s\n' "$1" | sed -n '1,/^__MS__$/p' | sed '$d'; }
splitMS() { printf '%s\n' "$1" | sed -n '/^__MS__$/,$p' | sed '1d' | head -1; }

waitForVideo() {
    start=$(now_ms)
    min_ms=$(( MIN_WAIT * 1000 ))
    timeout_ms=$(( TUNE_TIMEOUT * 1000 ))
    black_after_ms=$(( BLACK_AFTER * 1000 ))

    # Snapshot what the previous channel is still holding, so it can be told
    # apart from whatever this tune allocates.
    last_black=-99999
    _b=$(pollDevice)
    BASE_CODEC=$(secureCodecId "$(splitRM "$_b")")
    base_session=$(mediaSessionState "$(splitMS "$_b")")
    [ "$base_session" = "playing" ] && armed=0 || armed=1
    [ -n "$DEBUG" ] && log "baseline codec=${BASE_CODEC:-none} session=${base_session}"

    hits=0
    while :; do
        elapsed=$(( $(now_ms) - start ))
        if [ "$elapsed" -ge "$timeout_ms" ]; then
            log "no playback after $(( elapsed / 1000 ))s (codec=${codec_id:-none} base=${BASE_CODEC:-none} session=${ms_state:-unqueried})"
            return 1
        fi

        out=$(pollDevice)
        codec_id=$(secureCodecId "$(splitRM "$out")")
        ms_state=$(mediaSessionState "$(splitMS "$out")")

        # A decoder that is not the one we started with means new playback.
        if [ -n "$codec_id" ] && [ "$codec_id" != "$BASE_CODEC" ]; then
            state=playing
        else
            state="$ms_state"
            case "$BLACK_CHECK" in
                1) use_black=1 ;;
                0) use_black=0 ;;
                *) [ "$elapsed" -ge "$black_after_ms" ] && use_black=1 || use_black=0 ;;
            esac
            # Rate limited: the capture and decode cost about a second, so
            # unthrottled it would become the poll interval.
            if [ "$use_black" = "1" ] && [ "$state" != "playing" ] &&
               [ $(( elapsed - last_black )) -ge 2000 ]; then
                last_black=$elapsed
                blackScreenPlaying && { state=playing; via_black=1; }
            fi
        fi

        # The session cannot be told apart by identity, so it only counts once
        # it has dropped at least once since we started.
        [ "$state" != "playing" ] && armed=1

        [ -n "$DEBUG" ] && log "t=$(( elapsed / 100 ))ds codec=${codec_id:-none} base=${BASE_CODEC:-none} session=${ms_state} armed=${armed} hits=${hits} -> ${state}"

        if [ "$state" = "playing" ] && [ "$armed" = "1" ] && [ "$elapsed" -ge "$min_ms" ]; then
            hits=$(( hits + 1 ))
            if [ "$hits" -ge "$CONFIRM" ]; then
                sleep "$SETTLE"
                if [ -n "$codec_id" ] && [ "$codec_id" != "$BASE_CODEC" ]; then
                    _via="codec ${codec_id} (was ${BASE_CODEC:-none})"
                elif [ "$via_black" = "1" ]; then
                    _via="hdcp black screen"
                else
                    _via="session ${ms_state}"
                fi
                log "playback detected after $(( elapsed / 1000 ))s via ${_via}, ${hits} confirmations"
                return 0
            fi
        else
            hits=0
            via_black=
        fi

        if [ "$hits" -gt 0 ]; then
            sleep "$CONFIRM_POLL"
        else
            sleep "$POLL"
        fi
    done
}

if [ -n "$TUNER_IP" ]; then
    adb connect "$TUNER_IP" >/dev/null 2>&1
    if ! waitForVideo && [ "$ON_TIMEOUT" = "fail" ]; then
        log "failing the tune rather than streaming whatever is on screen"
        exit 1
    fi
else
    log "TUNER${n}_IP not set -- no gate"
fi

# curl, not ffmpeg: ffmpeg demuxes and analyses the input before it muxes
# anything out, ~5s of setup on this encoder, where curl is a byte pipe at
# ~0.2s. Same bytes either way -- the raw passthrough ah4c already does on its
# network-encoder path.
exec curl -s -N "$ENCODER_URL"
