//go:build linux && amd64

package main

import _ "embed"

//go:embed third_party/curl-amd64
var curlBin []byte
