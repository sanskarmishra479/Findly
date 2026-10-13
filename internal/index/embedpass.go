package index

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/store"
)

// EmbedProgress is reported while waiting passages get their vectors.
type EmbedProgress struct {
	Files    int // files finished in this pass
	Passages int // passages embedded (or copied) so far
	Waiting  int // passages that were waiting when the pass started
}

// EmbedStats summarize one embedding pass.
type EmbedStats struct {
	Files    int           // files whose passages got vectors
	Passages int           // passages sent to the embedding model
	Copied   int           // files that took the vectors of an identical file instead
	Failed   int           // files the model failed on (they stay keyword-searchable)
	Embed    time.Duration // time spent waiting for the model
}

// maxCrashFiles: if this many files in a row crash the embedding server, the server
// itself is broken (not the files), so the pass stops instead of failing every file.
const maxCrashFiles = 3

// EmbedPending adds vectors to every passage waiting for one, file by file in the order
// they were stored (the indexing priority order), until none is left or ctx ends. It is
// the second half of indexing: IndexRoot makes files keyword-searchable at once, and
// this makes them searchable by meaning. Interrupted, it simply continues next time.
//
// onProgress, if set, is called after each file and must return quickly.
func (ix *Indexer) EmbedPending(ctx context.Context, onProgress func(EmbedProgress)) (EmbedStats, error) {
	var st EmbedStats
	if ix.Embedder == nil {
		return st, errors.New("index: EmbedPending needs an Embedder")
	}
	waiting, err := ix.Store.CountPending(ctx)
	if err != nil {
		return st, err
	}
	prog := EmbedProgress{Waiting: waiting}
	report := func(n int) {
		prog.Files++
		prog.Passages += n
		if onProgress != nil {
			onProgress(prog)
		}
	}

	crashes := 0 // consecutive files that crashed the embedding server
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		f, ok, err := ix.Store.NextPendingFile(ctx)
		if err != nil || !ok {
			return st, err
		}

		// An identical file that already has its vectors: copy them instead.
		if f.Hash != "" {
			copied, err := ix.Store.CopyDuplicate(ctx, f.Hash, f.Status, f.ID, true)
			if err != nil {
				return st, err
			}
			if copied {
				st.Copied++
				report(0)
				continue
			}
		}

		items, err := ix.Store.PendingItems(ctx, f.ID)
		if err != nil {
			return st, err
		}
		if len(items) == 0 {
			continue // replaced or deleted meanwhile
		}
		texts := make([]string, len(items))
		for i, it := range items {
			texts[i] = it.Text
		}

		vecs, err := ix.embed(ctx, f.Name, texts, &st)
		if errors.Is(err, embed.ErrCrashed) {
			// Plan section 5: the server restarts automatically; try this file once more. If
			// it crashes again, this file is the likely cause: give up on it and continue.
			vecs, err = ix.embed(ctx, f.Name, texts, &st)
			if errors.Is(err, embed.ErrCrashed) {
				if crashes++; crashes >= maxCrashFiles {
					return st, fmt.Errorf("%w: %w", ErrEmbedder, err)
				}
				if err := ix.Store.MarkEmbedFailed(ctx, f.ID, "the embedding model crashed on this file twice"); err != nil {
					return st, err
				}
				st.Failed++
				report(0)
				continue
			}
		}
		switch {
		case err == nil:
		case ctx.Err() != nil:
			// Paused or quitting: report it as such, not as a broken server (which could make
			// the app switch backends).
			return st, ctx.Err()
		case errors.Is(err, embed.ErrInput):
			// About this input only: the file keeps its keyword search, the pass goes on.
			if err := ix.Store.MarkEmbedFailed(ctx, f.ID, err.Error()); err != nil {
				return st, err
			}
			st.Failed++
			report(0)
			continue
		default:
			// Server down, broken output, timeout: stop, and leave the passages waiting so
			// nothing is wrongly given up on. %w twice keeps ErrCrashed etc. visible.
			return st, fmt.Errorf("%w: %w", ErrEmbedder, err)
		}
		crashes = 0

		ivs := make([]store.ItemVector, len(items))
		for i, it := range items {
			v := vecs[i]
			ivs[i] = store.ItemVector{ID: it.ID, Vector: store.Vector{Bin: v.Bin, I8: v.Q.I8, Scale: v.Q.Scale}}
		}
		if _, err := ix.Store.SetVectors(ctx, ivs); err != nil {
			return st, err
		}
		st.Files++
		report(len(items))
	}
}

// embed asks the model for the vectors of one file's passages.
func (ix *Indexer) embed(ctx context.Context, title string, texts []string, st *EmbedStats) ([]embed.Vector, error) {
	start := time.Now()
	vecs, err := ix.Embedder.EmbedPassages(ctx, title, texts)
	st.Embed += time.Since(start)
	st.Passages += len(texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("got %d vectors for %d passages", len(vecs), len(texts))
	}
	return vecs, nil
}
