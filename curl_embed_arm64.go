//go:build linux && arm64

package main

import _ "embed"

//go:embed third_party/curl-arm64
var curlBin []byte
