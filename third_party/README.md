# Bundled static curl

These are the statically linked curl binaries embedded into streamgate at
build time (one per release architecture, selected by the `curl_embed_*.go`
build tags) and unpacked at runtime by `DELIVERY=curl`.

Upstream: curl 8.21.0, static musl builds from
<https://github.com/stunnel/static-curl/releases/tag/8.21.0> — the `curl`
binary extracted from each `curl-linux-<arch>-musl-8.21.0.tar.xz` and verified
against the SHA256SUMS shipped inside the same tarball.

| file | upstream arch | sha256 |
|---|---|---|
| `curl-amd64` | x86_64 | `153ca463957609117d21a848be29b70691b85f9e5cc9370c7daa037b839a4e45` |
| `curl-arm64` | aarch64 | `1cd1df1734854a9a427ea012e36306980f256c1a03afed1f1f552d790ff474ae` |
| `curl-armv7` | armv7 | `75b5315f086eb581173479da5ff4fab3f8bde6396fdd3e9c3946601c8e7f85dd` |

curl is distributed under the curl license — see `CURL_LICENSE`, fetched from
upstream's COPYING.

To update: download the new release's musl tarballs for x86_64/aarch64/armv7,
verify each `curl` against the SHA256SUMS inside its tarball, replace the
three files here, bump `curlVersion` in `curl.go`, and refresh the table
above.
