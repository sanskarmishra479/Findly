package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/extract"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/store"
)

// engineFlags are shared by the index and search commands.
type engineFlags struct {
	dataDir string
	backend string
	tpDir   string
}

func (e *engineFlags) register(fs *flag.FlagSet) {
	def, _ := appfs.DefaultDir()
	fs.StringVar(&e.dataDir, "data", def, "app data folder (index lives in <data>\\index)")
	fs.StringVar(&e.backend, "backend", "cpu", "llama.cpp build: cpu | vulkan")
	fs.StringVar(&e.tpDir, "third-party", "third_party", "folder with llama\\<backend> and models (from scripts\\fetch-*.ps1)")
}

// engine is an open index (and, when needed, a running embedding server).
type engine struct {
	fs    *appfs.FS
	store *store.Store
	emb   *embed.Server
}

func openEngine(e engineFlags) (*engine, error) {
	dir, err := filepath.Abs(e.dataDir)
	if err != nil {
		return nil, err
	}
	afs, err := appfs.Open(dir)
	if err != nil {
		return nil, err
	}
	dbPath, err := afs.Path("index/index.db")
	if err != nil {
		afs.Close()
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		afs.Close()
		return nil, err
	}
	return &engine{fs: afs, store: st}, nil
}

// startEmbedder launches llama-server with the text model.
func (en *engine) startEmbedder(ctx context.Context, e engineFlags, lowPriority bool) error {
	if e.backend != "cpu" && e.backend != "vulkan" {
		return fmt.Errorf("unknown backend %q", e.backend)
	}
	tp, err := filepath.Abs(e.tpDir)
	if err != nil {
		return err
	}
	cfg := embed.Config{
		ServerExe:   filepath.Join(tp, "llama", e.backend, "llama-server.exe"),
		Model:       filepath.Join(tp, "models", "embeddinggemma-2-Q8_0.gguf"),
		LowPriority: lowPriority,
	}
	for _, p := range []string{cfg.ServerExe, cfg.Model} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s (run scripts\\fetch-llama.ps1 and scripts\\fetch-model.ps1)", p)
		}
	}
	fmt.Fprintln(os.Stderr, "starting the embedding model…")
	srv, err := embed.Start(ctx, cfg) // ctx: Ctrl+C also stops a slow model start
	if err != nil {
		return err
	}
	en.emb = srv
	return nil
}

func (en *engine) close() {
	if en.emb != nil {
		en.emb.Close()
	}
	extract.ClosePDF()
	en.store.Close()
	en.fs.Close()
}

// removeThumbs deletes thumbnail files of removed index entries (P-5).
func (en *engine) removeThumbs(paths []string) {
	for _, p := range paths {
		// Paths come from the database. A corrupt value like "..\license\x" must not
		// delete anything outside the thumbnails folder (appfs alone only keeps it inside
		// the app folder).
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			fmt.Fprintln(os.Stderr, "warning: ignoring bad thumbnail path:", p)
			continue
		}
		if err := en.fs.Remove(filepath.Join(appfs.DirThumbs, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "warning: could not remove thumbnail:", err)
		}
	}
}

// absFolder turns a user-typed folder into the absolute, real form stored in the index,
// so "docs", "C:" or a path with a trailing backslash still match.
func absFolder(p string) (string, error) {
	if len(p) == 2 && p[1] == ':' {
		p += `\` // "C:" means the drive root, not the current folder on C:
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := safefs.Canonical(abs); err == nil {
		return real, nil
	}
	return abs, nil // a folder that no longer exists (e.g. for remove)
}
