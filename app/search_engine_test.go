package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/engine"
	"github.com/sanskarmishra479/findly/internal/safefs"
)

// The panel's Search and Preview against a real engine and index (without the model:
// words only), the way the UI calls them.
func TestSearchAndPreviewEndToEnd(t *testing.T) {
	fs, err := appfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	e, err := engine.Open(engine.Config{FS: fs, Licensed: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	dir, _ := safefs.Canonical(t.TempDir())
	os.WriteFile(filepath.Join(dir, "plumber.txt"), []byte("The plumber fixed the leaking kitchen tap on Tuesday."), 0o644)
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("Nothing to see here."), 0o644)
	if _, err := e.AddFolder(context.Background(), dir); err != nil {
		t.Fatal(err)
	}

	a := newApp(fs)
	ctx := context.Background()
	a.ctx.Store(&ctx)
	a.engine = e

	var resp SearchResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp = a.Search(7, "leaking tap", false)
		if len(resp.Results) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp.Seq != 7 || len(resp.Results) != 1 || resp.Results[0].Name != "plumber.txt" {
		t.Fatalf("search = %+v", resp)
	}
	r := resp.Results[0]
	if !r.Keyword || r.ItemID == 0 || !strings.Contains(r.Snippet, "leaking") || strings.Join(resp.Words, ",") != "leaking,tap" {
		t.Fatalf("result = %+v, words %v", r, resp.Words)
	}
	if !strings.Contains(r.Folder, filepath.Base(dir)) {
		t.Errorf("folder = %q", r.Folder)
	}

	// Meaning search without a model: words still come back, with the reason.
	m := a.Search(8, "leaking tap", true)
	if len(m.Results) != 1 || m.Err != "" {
		t.Fatalf("meaning search without a model = %+v", m)
	}

	p := a.Preview(3, r.ID, r.ItemID, "leaking tap")
	if p.Seq != 3 || !strings.Contains(p.Text, "plumber fixed") || p.Image != "" {
		t.Fatalf("preview = %+v", p)
	}
	if bad := a.Preview(4, 999999, 0, "x"); bad.Err == "" {
		t.Fatal("a preview of an unknown file gave no message")
	}
	if msg := a.OpenResult(999999, 0); msg == "" {
		t.Fatal("opening an unknown file gave no message")
	}
}
