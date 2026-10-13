//go:build !devpaths

package main

// devPaths is off in release builds: an environment variable must never choose which
// llama-server.exe Findly runs.
const devPaths = false
