//go:build !gocv

package pipeline

// gocvAvailable is false in a build without OpenCV.
//
// A constant rather than a runtime check, so that what this binary can open is decided at compile time and
// reported honestly. The alternative — offering a source and failing to open it — is worse than it sounds: a
// receiver that has persisted an operator's choice of an unavailable source will not start, which is how a
// settings page came to be able to stop the service.
const gocvAvailable = false
