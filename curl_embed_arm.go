//go:build linux && arm

package main

import _ "embed"

//go:embed third_party/curl-armv7
var curlBin []byte
