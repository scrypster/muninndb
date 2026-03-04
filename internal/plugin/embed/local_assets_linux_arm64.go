//go:build linux && arm64 && localassets

package embed

import _ "embed"

// nativeLibFilename is the extracted filename for the ORT shared library on this platform.
const nativeLibFilename = "libonnxruntime.so"

// embeddedNativeLib is the ORT 1.24.2 shared library for linux/arm64.
// Populated by `make fetch-assets`.
//
//go:embed assets/libonnxruntime_linux_arm64.so
var embeddedNativeLib []byte
