package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/engine"
)

// Model files. The app looks in its data folder (where the T6 download puts the model)
// and next to the exe (where the installer puts llama-server). In development builds
// (-tags devpaths) FINDLY_DEV_THIRD_PARTY can point at the repository's third_party
// folder (scripts\fetch-*.ps1).
const (
	modelFile  = "embeddinggemma-2-Q8_0.gguf"
	llamaExe   = "llama-server.exe"
	devEnvVar  = "FINDLY_DEV_THIRD_PARTY"
	cpuBackend = "cpu" // T8 adds the Vulkan/CPU choice
)

// modelStarter returns how to start the embedding model, or nil (keyword search only)
// when its files are not there yet. The returned text says what was found, for About.
func modelStarter(fs *appfs.FS) (engine.StartModel, string) {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	models, _ := fs.Path(appfs.DirModels)
	dev := ""
	if devPaths {
		dev = os.Getenv(devEnvVar)
	}

	server := firstExisting(
		filepath.Join(exeDir, "llama", cpuBackend, llamaExe),
		devPath(dev, "llama", cpuBackend, llamaExe),
	)
	model := firstExisting(
		filepath.Join(models, modelFile),
		devPath(dev, "models", modelFile),
	)
	switch {
	case server == "":
		return nil, "the search model program (llama-server) was not found: keyword search only"
	case model == "":
		return nil, "the search model is not downloaded yet: keyword search only"
	}
	gpu, err := fs.Path(appfs.DirGPU)
	if err != nil {
		return nil, err.Error()
	}
	start := func(ctx context.Context, lowPriority bool) (engine.Model, error) {
		return embed.Start(ctx, embed.Config{
			ServerExe: server, Model: model, WorkDir: gpu, LowPriority: lowPriority,
		})
	}
	return start, "search model: " + model
}

func devPath(root string, parts ...string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// tooltip is the tray text for a status (Windows shows at most 127 characters).
func tooltip(s engine.Status) string {
	switch s.State {
	case engine.StateIndexing:
		if s.Total > 0 {
			return fmt.Sprintf("Findly: reading files %s / %s in %s", thousands(s.Done), thousands(s.Total), filepath.Base(s.Root))
		}
		return "Findly: looking for new files in " + filepath.Base(s.Root)
	case engine.StateEmbedding:
		if s.Waiting > 0 {
			return fmt.Sprintf("Findly: preparing meaning search %d%%", min(99, s.Passages*100/s.Waiting))
		}
		return "Findly: preparing meaning search"
	case engine.StatePaused:
		return "Findly: indexing paused"
	case engine.StateError:
		return "Findly: indexing stopped after a problem; retrying later"
	default:
		return "Findly: ready. Ctrl+Shift+Space to search"
	}
}

// thousands formats 4210 as "4,210".
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// statusThrottle forwards at most one status every interval (progress arrives per file),
// always passing state changes through at once so "ready" or "paused" never lags.
type statusThrottle struct {
	interval time.Duration
	last     time.Time
	state    engine.State
}

func (t *statusThrottle) pass(s engine.Status, now time.Time) bool {
	if s.State != t.state || now.Sub(t.last) >= t.interval {
		t.state, t.last = s.State, now
		return true
	}
	return false
}
