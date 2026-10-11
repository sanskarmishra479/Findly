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
	"strings"
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
	ModelDim      = 768      // EmbeddingGemma 2 output size; anything else is the wrong model
	BatchSize     = 16       // passages per request (plan 9.8; tune with cli bench)
	MaxQueryRunes = 500      // longer queries are cut
	cacheSize     = 256      // recent query vectors kept (plan 9.8)
	maxRestarts   = 3        // plan 5: restart a crashed sidecar at most 3 times in a row
	maxResponse   = 64 << 20 // response bytes read from the server
	maxHalvings   = 3        // a too-long passage is cut in half at most this often
	startTimeout  = 3 * time.Minute
	launchTries   = 3 // new port per try if another program took ours
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

var (
	// ErrServer means the embedding server is not working (crashed, broken output, not
	// reachable). The indexer stops instead of blaming the file.
	ErrServer = errors.New("embed: embedding server unavailable")
	// ErrInput means this particular input could not be embedded (e.g. too long even
	// after cutting). The indexer marks the file failed and continues.
	ErrInput = errors.New("embed: input cannot be embedded")
)

// Server is a running llama-server. Safe for concurrent use.
type Server struct {
	cfg    Config
	client *http.Client
	life   context.Context // cancelled by Close; startup uses it, never a caller's ctx
	stop   context.CancelFunc

	mu       sync.Mutex
	proc     *sidecar.Proc
	base     string // http://127.0.0.1:port
	key      string
	log      *sidecar.Tail
	restarts int // consecutive crashes; reset by a successful request

	cacheMu sync.Mutex
	cache   map[string]*list.Element
	order   *list.List // front = most recent
}

type cacheEntry struct {
	query string
	vec   Vector
}

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
	life, stop := context.WithCancel(context.Background())
	s := &Server{
		cfg:  cfg,
		life: life,
		stop: stop,
		client: &http.Client{
			// Only ever talks to our own loopback server: never through a proxy.
			Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4},
			Timeout:   5 * time.Minute,
		},
		cache: map[string]*list.Element{},
		order: list.New(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The caller's ctx only bounds how long Start waits.
	done := make(chan error, 1)
	go func() { done <- s.launch() }()
	select {
	case err := <-done:
		if err != nil {
			stop()
			return nil, err
		}
		return s, nil
	case <-ctx.Done():
		stop()
		<-done
		return nil, ctx.Err()
	}
}

// launch starts a fresh process (stopping any old one); s.mu must be held.
func (s *Server) launch() error {
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
	var lastErr error
	for range launchTries {
		lastErr = s.launchOnce()
		if lastErr == nil || !errors.Is(lastErr, errPortTaken) {
			return lastErr
		}
	}
	return lastErr
}

var errPortTaken = errors.New("embed: port taken by another program")

func (s *Server) launchOnce() error {
	port, err := pickPort()
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
		"--no-ui",
		"--threads", strconv.Itoa(s.cfg.Threads),
		// Two slots of 2,048 tokens: enough for a passage, an image or 30 s of audio, and
		// keeps RAM near 400 MB instead of 760 MB with the defaults.
		"--ctx-size", "4096",
		"--parallel", "2",
		"--batch-size", "2048",
		"--ubatch-size", "2048", // a whole input must fit in one batch for embeddings
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
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithTimeout(s.life, startTimeout)
	defer cancel()
	for {
		if exited, _ := proc.Exited(); exited {
			proc.Stop()
			if strings.Contains(s.log.String(), "bind") {
				return errPortTaken
			}
			return fmt.Errorf("%w: llama-server exited during startup: %s", ErrServer, shorten(s.log.String(), 2000))
		}
		// Before sending anything (even a health check) make sure the port belongs to
		// our process, so the API key and text never reach another program (P-4).
		if pid, err := listenerPID(port); err == nil {
			if pid != uint32(proc.Pid()) {
				proc.Stop()
				return errPortTaken
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
			if resp, err := s.client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					s.proc, s.base, s.key = proc, base, key
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			proc.Stop()
			if s.life.Err() != nil {
				return fmt.Errorf("%w: closed", ErrServer)
			}
			return fmt.Errorf("%w: not ready after %v", ErrServer, startTimeout)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Close stops the server. It does not wait for a startup in progress: that startup
// sees the cancelled life context and stops its own process.
func (s *Server) Close() {
	s.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
}

// endpoint returns the server address and key, restarting a crashed server.
func (s *Server) endpoint() (base, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.life.Err() != nil {
		return "", "", fmt.Errorf("%w: closed", ErrServer)
	}
	exited := s.proc == nil
	if !exited {
		exited, _ = s.proc.Exited()
	}
	if exited {
		if s.restarts >= maxRestarts {
			return "", "", fmt.Errorf("%w: crashed %d times in a row", ErrServer, s.restarts)
		}
		s.restarts++
		if err := s.launch(); err != nil {
			return "", "", err
		}
	}
	return s.base, s.key, nil
}

func (s *Server) succeeded() {
	s.mu.Lock()
	s.restarts = 0
	s.mu.Unlock()
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

// EmbedPassages embeds passages of one file (title = file name), in batches. If a batch
// is rejected because an input is too long, its passages are retried one at a time and
// a too-long passage is cut in half (up to maxHalvings times) before giving up.
func (s *Server) EmbedPassages(ctx context.Context, title string, texts []string) ([]Vector, error) {
	out := make([]Vector, 0, len(texts))
	for start := 0; start < len(texts); start += BatchSize {
		batch := texts[start:min(start+BatchSize, len(texts))]
		inputs := make([]string, len(batch))
		for i, t := range batch {
			inputs[i] = PassageText(title, t)
		}
		vs, err := s.embed(ctx, inputs)
		if errors.Is(err, ErrInput) && len(batch) > 1 {
			vs = vs[:0]
			for _, t := range batch {
				v, err := s.embedOne(ctx, title, t)
				if err != nil {
					return nil, err
				}
				vs = append(vs, v)
			}
		} else if errors.Is(err, ErrInput) {
			v, err := s.embedOne(ctx, title, batch[0])
			if err != nil {
				return nil, err
			}
			vs = []Vector{v}
		} else if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

func (s *Server) embedOne(ctx context.Context, title, text string) (Vector, error) {
	for range maxHalvings + 1 {
		vs, err := s.embed(ctx, []string{PassageText(title, text)})
		if err == nil {
			return vs[0], nil
		}
		if !errors.Is(err, ErrInput) {
			return Vector{}, err
		}
		// The server says how many tokens the input had; cut to fit with some margin.
		// Without that number, halve.
		keep := len(text) / 2
		if n := tokensInError(err.Error()); n > 0 {
			keep = min(keep, len(text)*1800/n)
		}
		text = cutBytes(text, keep)
	}
	return Vector{}, fmt.Errorf("%w: passage still too long after cutting", ErrInput)
}

// embed sends inputs to POST /v1/embeddings and converts the results.
func (s *Server) embed(ctx context.Context, inputs []string) ([]Vector, error) {
	base, key, err := s.endpoint()
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
		if ctx.Err() != nil {
			return nil, ctx.Err() // the caller gave up; not the server's fault
		}
		return nil, fmt.Errorf("%w: request: %v", ErrServer, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrServer, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := shorten(string(raw), 300)
		if strings.Contains(msg, "too large") || strings.Contains(msg, "exceed") {
			return nil, fmt.Errorf("%w: %s", ErrInput, msg)
		}
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrServer, resp.StatusCode, msg)
	}
	vs, err := parseResponse(raw, len(inputs))
	if err != nil {
		// Wrong count, NaN or wrong size: the server or backend is broken, not the file.
		return nil, fmt.Errorf("%w: %v", ErrServer, err)
	}
	s.succeeded()
	return vs, nil
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
		if len(d.Embedding) != ModelDim {
			return nil, fmt.Errorf("embed: vector has %d dimensions, want %d (wrong model?)", len(d.Embedding), ModelDim)
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

// pickPort chooses a free port (a variable so tests can simulate a squatter).
var pickPort = freePort

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
	return cutBytes(s, n) + "…"
}

// cutBytes shortens s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tokensInError reads N from llama-server's "input (N tokens) is too large" message.
func tokensInError(msg string) int {
	_, after, ok := strings.Cut(msg, "input (")
	if !ok {
		return 0
	}
	num, _, ok := strings.Cut(after, " tokens)")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
