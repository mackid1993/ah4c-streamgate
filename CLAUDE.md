# Development notes

streamgate is an ah4c `CMDn` helper: it gates the encoder stream on the Android
box actually playing video. Everything user-facing lives in README.md — keep it
that way. The README is for end users; build, test and release mechanics belong
here.

## Build

```sh
go build -o streamgate .
```

Standard library only; Go 1.21 or later. `--version` reports the build stamp
(`dev` for an unstamped build). Release binaries mirror what CI does:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o streamgate .
```

`GOARCH=arm64`, or `GOARCH=arm GOARM=7`, for the other release targets. CGO off
so the binary is static and runs in the ah4c container without a libc
dependency.

## Tests

```sh
go test -race -count=1 ./...
```

The suite needs macOS or Linux. `adb_harness_test.go` fakes `adb` with a
`#!/bin/sh` script installed first on `PATH`, so on Windows every harness test
fails by construction (`adbShell` gets nothing back) — those failures are
environmental, not regressions. The parser and streaming tests in
`main_test.go` pass anywhere. On a Windows checkout `gofmt -l` also flags every
file, because `core.autocrlf` puts CRLF in the working tree; verify formatting
against LF-normalised copies — the committed content is LF and CI's gofmt gate
is the authority.

## Bundled curl and stall filling

The encoder is fetched by a bundled static curl, not by `net/http`: curl gets
the HTTP, streamgate gets the bytes. `openViaCurl` gives curl an `os.Pipe` for
stdout rather than fd 1 — that distinction is load-bearing. Inheriting fd 1
would deliver the encoder's pre-gate buffer verbatim, splash screen and all,
with nothing left in the process able to discard it, and would make stall
filling impossible. Staying in the byte path is what pays for keyframe
alignment and the pre-gate discard.

`openStream` is a var so the in-process streaming tests can substitute
`openViaHTTP` (the retained `net/http` source) and run on any platform; the
`init` in `main_test.go` deliberately leaves it alone under `SG_SUBPROCESS=1`,
so the harness exercises the real curl path.

`idleReader` enforces `READ_TIMEOUT` over curl's pipe (the socket deadline the
in-process path uses is not available there) and logs any encoder silence past
`gapReport`. It synthesises nothing.

**streamgate passes data through and must stay that way.** It does not buffer,
pad, pace or manufacture packets -- that is the maintainer's explicit
constraint, not an implementation detail. Two attempts to add bytes have
already been reverted:

- Null-filling gaps. Wrong twice over. Injected packets went through
  `readPacket` like real ones, so synthetic data drove streamgate's own state
  machine -- the `ALIGN_TIMEOUT` fallback fired during a silence instead of
  waiting for the encoder, which `TestNoVideoMeansNoBytes` caught. And a null
  packet carries no frame, so filling a gap cannot stop a player stuttering
  through it. ah4c handles null insertion itself; streamgate must not
  second-guess it.
- Adding read/emit slack to smooth jitter. Rejected outright: it puts
  streamgate between the user and live TV.

Do not make claims in this repo about ah4c's internals. Several have been
asserted here and been wrong; the maintainer wrote that code and is the
authority on it. Likewise, do not attribute the reported buffering to any
cause without a measurement -- a lost "cushion" and late-vs-early connect were
each built up and then disproven by his own evidence. The gap log exists to
produce the missing measurement: a stutter with no gap line rules out the
encoder going quiet.

`third_party/` carries pinned static curl binaries (musl builds; provenance and
checksums in `third_party/README.md`), embedded per release architecture by the
build-tagged `curl_embed_*.go` files and unpacked at tune time to a stable path
under `TempDir` so repeated tunes do not each leave a 10MB file behind.
Non-linux builds embed nothing and fail loudly at stream time. To update: fetch
the new release's musl tarballs, verify each `curl` against the SHA256SUMS
inside its tarball, replace the binaries, bump `curlVersion` in `curl.go`, and
refresh the provenance table. `TestBundledCurlRuns` execs the embedded binary,
so it only runs on linux — one more reason the manual CI dispatch is the
release gate.

## CI and releases

`.github/workflows/build.yml` runs on `workflow_dispatch` only — nothing runs
on push. The maintainer triggers it from the Actions tab. It gates on gofmt,
`go vet`, and the race-enabled suite against both the declared minimum Go
version and current stable, then cross-builds the three linux binaries and
smoke-tests the amd64 one. Setting the `release_tag` input additionally
publishes a release with a `SHA256SUMS` manifest; leaving it empty just checks
and builds. Because the harness cannot run on Windows, a manual CI dispatch is
the full-suite verification for changes made on a Windows machine — part of the
release ritual, not optional.

## Conventions

- stdout is the video stream. Nothing but stream bytes may ever be written to
  it; all logging goes to stderr. `stdoutW` exists so tests can substitute a
  buffer.
- Log lines must never misstate what the program did — they are what users
  paste into support threads. The same goes for comments: they carry the
  field-verified *why*, and several encode hardware behaviour that cannot be
  re-derived from the code.
- One idea per commit; the message states the behavioural claim the commit
  makes.
