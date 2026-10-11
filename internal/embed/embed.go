//go:build windows

// Package embed turns text into vectors with EmbeddingGemma 2 running in llama-server
// (plan 9.8). The server runs as a sidecar bound to 127.0.0.1 on a random port, with a
// random API key, no network access, no web UI and a clean environment (plan P-4).
package embed

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sanskarmishra479/findly/internal/sidecar"
	"github.com/sanskarmishra479/findly/internal/vecfmt"
)

// Prefixes from the EmbeddingGemma 2 model card. Queries and passages must use them, or
// search quality drops noticeably.
func QueryText(q string) string { return "task: search result | query: " + q }

// PassageText formats a document passage; the file name serves as the title.
func PassageText(title, text string) string {
	if title == "" {
		title = "none"
	}
	return "title: " + title + " | text: " + text
}

const (
	BatchSize     = 16       // passages per request (plan 9.8; tune with cli bench)
	MaxQueryRunes = 500      // longer queries are cut
	cacheSize     = 256      // recent query vectors kept (plan 9.8)
	maxRestarts   = 3        // plan 5: restart a crashed sidecar at most 3 times
	maxResponse   = 64 << 20 // response bytes read from the server
	startTimeout  = 3 * time.Minute
)

// Config says where the server and model files are. All paths must be absolute.
type Config struct {
	ServerExe   string // llama-server.exe (CPU or Vulkan build)
	Model       string // text model .gguf
	MMProj      string // vision+audio projector .gguf; empty = text only
	Threads     int    // 0 = half the CPU cores
	LowPriority bool   // below-normal priority while indexing
}

// Vector is one embedding in both stored forms.
type Vector struct {
	Bin vecfmt.Bin
	Q   vecfmt.Int8
}

// Server is a running llama-server. Safe for concurrent use.
type Server struct {
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	proc     *sidecar.Proc
	base     string // http://127.0.0.1:port
	key      string
	log      *sidecar.Tail
	restarts int

	cacheMu sync.Mutex
	cache   map[string]*list.Element
	order   *list.List // front = most recent
}

type cacheEntry struct {
	query string
	vec   Vector
}

// ErrServer is returned when the embedding server cannot be (re)started.
var ErrServer = errors.New("embed: embedding server unavailable")

// Start launches llama-server and waits until it is ready.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	for _, p := range []string{cfg.ServerExe, cfg.Model} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("embed: path %q must be absolute", p)
		}
	}
	if cfg.MMProj != "" && !filepath.IsAbs(cfg.MMProj) {
		return nil, fmt.Errorf("embed: path %q must be absolute", cfg.MMProj)
	}
	if cfg.Threads <= 0 {
		cfg.Threads = max(1, runtime.NumCPU()/2)
	}
	s := &Server{
		cfg: cfg,
		client: &http.Client{
			// Only ever talks to our own loopback server: no proxy, no keep-alive surprises.
			Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4},
			Timeout:   5 * time.Minute,
		},
		cache: map[string]*list.Element{},
		order: list.New(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.launch(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// launch starts the process; s.mu must be held.
func (s *Server) launch(ctx context.Context) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	key, err := randomHex(24)
	if err != nil {
		return err
	}
	args := []string{
		"--model", s.cfg.Model,
		"--embeddings",
		"--host", "127.0.0.1", // loopback only
		"--port", strconv.Itoa(port),
		"--offline",        // never touch the network
		"--no-mmproj-auto", // never download a projector
		"--no-webui",
		"--threads", strconv.Itoa(s.cfg.Threads),
		"--batch-size", "2048",
		"--ubatch-size", "2048", // a whole passage (or 30 s of audio) must fit in one batch
	}
	if s.cfg.MMProj != "" {
		args = append(args, "--mmproj", s.cfg.MMProj)
	}
	s.log = &sidecar.Tail{Size: 16 << 10}
	proc, err := sidecar.Start(s.cfg.ServerExe, args, sidecar.Options{
		// API key via env, not argv, so it does not show in process listings. Nothing else
		// is passed, so no LLAMA_ARG_* variable can enable network or file tools.
		Env:         []string{"PATH=" + filepath.Dir(s.cfg.ServerExe), "LLAMA_API_KEY=" + key},
		LowPriority: s.cfg.LowPriority,
		Output:      s.log,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServer, err)
	}
	s.proc, s.base, s.key = proc, "http://127.0.0.1:"+strconv.Itoa(port), key

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	for {
		if exited, _ := proc.Exited(); exited {
			return fmt.Errorf("%w: llama-server exited during startup: %s", ErrServer, s.log.String())
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/health", nil)
		if resp, err := s.client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			proc.Stop()
			return fmt.Errorf("%w: not ready after %v", ErrServer, startTimeout)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
}

// endpoint returns the server address and key, restarting a crashed server.
func (s *Server) endpoint(ctx context.Context) (base, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return "", "", fmt.Errorf("%w: closed", ErrServer)
	}
	if exited, _ := s.proc.Exited(); exited {
		if s.restarts >= maxRestarts {
			return "", "", fmt.Errorf("%w: crashed %d times", ErrServer, s.restarts)
		}
		s.restarts++
		if err := s.launch(ctx); err != nil {
			return "", "", err
		}
	}
	return s.base, s.key, nil
}

// EmbedQuery embeds a search query (with the query prefix). Recent queries are cached.
func (s *Server) EmbedQuery(ctx context.Context, q string) (Vector, error) {
	if utf8.RuneCountInString(q) > MaxQueryRunes {
		q = string([]rune(q)[:MaxQueryRunes])
	}
	if v, ok := s.cached(q); ok {
		return v, nil
	}
	vs, err := s.embed(ctx, []string{QueryText(q)})
	if err != nil {
		return Vector{}, err
	}
	s.remember(q, vs[0])
	return vs[0], nil
}

// EmbedPassages embeds passages of one file (title = file name), in batches.
func (s *Server) EmbedPassages(ctx context.Context, title string, texts []string) ([]Vector, error) {
	out := make([]Vector, 0, len(texts))
	for start := 0; start < len(texts); start += BatchSize {
		batch := texts[start:min(start+BatchSize, len(texts))]
		inputs := make([]string, len(batch))
		for i, t := range batch {
			inputs[i] = PassageText(title, t)
		}
		vs, err := s.embed(ctx, inputs)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

// embed sends inputs to POST /v1/embeddings and converts the results.
func (s *Server) embed(ctx context.Context, inputs []string) ([]Vector, error) {
	base, key, err := s.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"model": "embeddinggemma-2", "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: server returned HTTP %d: %s", resp.StatusCode, shorten(string(raw), 300))
	}
	return parseResponse(raw, len(inputs))
}

func parseResponse(raw []byte, want int) ([]Vector, error) {
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embed: bad response: %w", err)
	}
	if len(parsed.Data) != want {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(parsed.Data), want)
	}
	out := make([]Vector, want)
	seen := make([]bool, want)
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= want || seen[d.Index] {
			return nil, fmt.Errorf("embed: bad vector index %d", d.Index)
		}
		seen[d.Index] = true
		bin, q, err := vecfmt.Encode(d.Embedding)
		if err != nil {
			return nil, err // NaN/Inf/zero: a broken backend (plan 14.2)
		}
		out[d.Index] = Vector{Bin: bin, Q: q}
	}
	return out, nil
}

func (s *Server) cached(q string) (Vector, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if e, ok := s.cache[q]; ok {
		s.order.MoveToFront(e)
		return e.Value.(cacheEntry).vec, true
	}
	return Vector{}, false
}

func (s *Server) remember(q string, v Vector) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if _, ok := s.cache[q]; ok {
		return
	}
	s.cache[q] = s.order.PushFront(cacheEntry{q, v})
	if s.order.Len() > cacheSize {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.cache, oldest.Value.(cacheEntry).query)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
