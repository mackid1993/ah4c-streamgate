//go:build !linux || (!amd64 && !arm64 && !arm)

package main

// No curl is bundled outside the linux release targets, and delivery is
// curl-only -- so a non-linux build fails loudly at stream time rather than
// pretending it can deliver.
var curlBin []byte
