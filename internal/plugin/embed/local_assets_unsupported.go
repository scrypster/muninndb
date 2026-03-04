//go:build localassets && !(darwin && arm64) && !(darwin && amd64) && !(linux && amd64) && !(linux && arm64) && !(windows && amd64)

package embed

// nativeLibFilename is empty on unsupported platforms; LocalProvider.Init() will
// return an error explaining that the bundled embedder is not available here.
const nativeLibFilename = ""

// embeddedNativeLib is nil on unsupported platforms.
var embeddedNativeLib []byte
