//go:build windows && amd64

package embed

import _ "embed"

// nativeLibFilename is the extracted filename for the ORT shared library on this platform.
const nativeLibFilename = "onnxruntime.dll"

// embeddedNativeLib is the ORT 1.24.2 shared library for windows/amd64.
// Populated by `make fetch-assets`.
//
//go:embed assets/onnxruntime_windows_amd64.dll
var embeddedNativeLib []byte
