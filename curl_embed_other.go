//go:build !linux || (!amd64 && !arm64 && !arm)

package main

// No curl is bundled outside the linux release targets; delivery fails loudly
// at runtime rather than shipping a binary that cannot run.
var curlBin []byte
