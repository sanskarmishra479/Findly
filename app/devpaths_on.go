//go:build devpaths

package main

// devPaths: this is a development build (`wails build -tags devpaths`), so the model
// may be taken from the repository (FINDLY_DEV_THIRD_PARTY).
const devPaths = true
