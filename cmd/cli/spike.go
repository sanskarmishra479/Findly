package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// spikeInput is one thing to embed: a text, an image file or an audio file.
type spikeInput struct {
	Name string // short label used in the output table
	Kind string // "text" | "image" | "audio"
	Text string // for text: the full string sent to the model (prefix included)
	Path string // for image/audio: file on disk
}

func runSpike(args []string) error {
	fs := flag.NewFlagSet("spike", flag.ExitOnError)
	backend := fs.String("backend", "cpu", "llama.cpp build to use: cpu | vulkan")
	// Not "-data": in every other command that is the app data folder.
	mediaDir := fs.String("media", `testdata\spike`, "folder with the test photos and audio")
	out := fs.String("out", "", "optional: write the 256-d vectors as JSON to this file")
	imgTokens := fs.Int("image-tokens", 0, "image token budget (70..1120); 0 = model default")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dog, cat, audio, err := findSpikeFiles(*mediaDir)
	if err != nil {
		return err
	}

	inputs := []spikeInput{
		{Name: "q:dog-beach", Kind: "text", Text: queryPrefix("a dog on the beach")},
		{Name: "q:cat", Kind: "text", Text: queryPrefix("a cat sitting outside")},
		{Name: "q:sleep", Kind: "text", Text: queryPrefix("where they talk about sleep")},
		{Name: "img:dog", Kind: "image", Path: dog},
		{Name: "img:cat", Kind: "image", Path: cat},
		{Name: "audio", Kind: "audio", Path: audio},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	srv, err := startLlamaServer(ctx, *backend, *imgTokens)
	if err != nil {
		return err
	}
	defer srv.Stop()

	vecs := make(map[string][]float32)
	for _, in := range inputs {
		start := time.Now()
		v, err := srv.Embed(ctx, in)
		if err != nil {
			return fmt.Errorf("embed %s: %w", in.Name, err)
		}
		fmt.Printf("%-12s %4d dims  %v\n", in.Name, len(v), time.Since(start).Round(time.Millisecond))
		vecs[in.Name] = truncateNormalize(v, 256)
	}

	fmt.Println()
	fmt.Println("cosine similarity (256-d):")
	pairs := [][2]string{
		{"q:dog-beach", "img:dog"}, {"q:dog-beach", "img:cat"},
		{"q:cat", "img:cat"}, {"q:cat", "img:dog"},
		{"q:sleep", "audio"}, {"q:dog-beach", "audio"},
	}
	for _, p := range pairs {
		fmt.Printf("  %-12s vs %-8s %.4f\n", p[0], p[1], cosine(vecs[p[0]], vecs[p[1]]))
	}

	fmt.Println()
	check := func(ok bool, msg string) {
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %s\n", mark, msg)
	}
	check(cosine(vecs["q:dog-beach"], vecs["img:dog"]) > cosine(vecs["q:dog-beach"], vecs["img:cat"]), "dog text is closer to dog photo than cat photo")
	check(cosine(vecs["q:cat"], vecs["img:cat"]) > cosine(vecs["q:cat"], vecs["img:dog"]), "cat text is closer to cat photo than dog photo")
	check(cosine(vecs["q:sleep"], vecs["audio"]) > cosine(vecs["q:dog-beach"], vecs["audio"]), "sleep text is closer to the audio than dog text")

	if *out != "" {
		b, err := json.MarshalIndent(map[string]any{"backend": *backend, "dim": 256, "vectors": vecs}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Println("\nvectors written to", *out)
	}
	return nil
}

func queryPrefix(q string) string { return "task: search result | query: " + q }

// findSpikeFiles picks the dog photo, cat photo and audio clip from the test folder by name.
func findSpikeFiles(dir string) (dog, cat, audio string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", "", err
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		full := filepath.Join(dir, e.Name())
		switch ext := filepath.Ext(name); {
		case ext == ".mp3" || ext == ".wav":
			audio = full
		case strings.Contains(name, "retriever") || strings.Contains(name, "dog"):
			dog = full
		case strings.Contains(name, "tabby") || strings.Contains(name, "cat"):
			cat = full
		}
	}
	if dog == "" || cat == "" || audio == "" {
		return "", "", "", fmt.Errorf("need a dog photo, a cat photo and an mp3/wav in %s", dir)
	}
	return dog, cat, audio, nil
}

// llamaServer is a running llama-server child process.
type llamaServer struct {
	cmd    *exec.Cmd
	base   string // e.g. http://127.0.0.1:53211
	apiKey string
	client *http.Client
}

func startLlamaServer(ctx context.Context, backend string, imgTokens int) (*llamaServer, error) {
	if backend != "cpu" && backend != "vulkan" {
		return nil, fmt.Errorf("unknown backend %q", backend)
	}
	exe := filepath.Join("third_party", "llama", backend, "llama-server.exe")
	textModel := filepath.Join("third_party", "models", "embeddinggemma-2-Q8_0.gguf")
	mmproj := filepath.Join("third_party", "models", "mmproj-embeddinggemma-2-Q8_0.gguf")
	for _, p := range []string{exe, textModel, mmproj} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("missing %s (run scripts\\fetch-llama.ps1 and scripts\\fetch-model.ps1): %w", p, err)
		}
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	key, err := randomHex(16)
	if err != nil {
		return nil, err
	}

	args := []string{
		"--model", textModel,
		"--mmproj", mmproj,
		"--embeddings",
		"--host", "127.0.0.1", // P-4: loopback only
		"--port", strconv.Itoa(port),
		"--offline",        // never touch the network
		"--no-mmproj-auto", // never auto-download a projector
		"--no-ui",          // no browser UI
		"--batch-size", "2048",
		"--ubatch-size", "2048", // embeddings need the whole input in one batch (30 s audio ≈ 750 tokens)
	}
	if imgTokens > 0 {
		args = append(args, "--image-min-tokens", strconv.Itoa(imgTokens), "--image-max-tokens", strconv.Itoa(imgTokens))
	}

	cmd := exec.CommandContext(ctx, exe, args...)
	// Clean environment: llama-server reads LLAMA_ARG_* / HF_TOKEN from env, which could
	// otherwise switch on network access or its file-reading tools. Pass only what Windows needs,
	// plus the API key (env, not argv, so it doesn't show up in process listings).
	// GPU drivers write logs/caches into the working folder and LOCALAPPDATA: keep them in
	// a temporary folder instead of next to llama-server.exe.
	work, err := os.MkdirTemp("", "findly-spike-gpu-")
	if err != nil {
		return nil, err
	}
	cmd.Dir = work
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"PATH=" + filepath.Dir(exe),
		"LLAMA_API_KEY=" + key,
		"LOCALAPPDATA=" + work, "APPDATA=" + work, "TEMP=" + work, "TMP=" + work,
	}
	logPath := filepath.Join(os.TempDir(), "findly-spike-llama-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start llama-server: %w", err)
	}
	go func() { cmd.Wait(); logFile.Close(); os.RemoveAll(work) }() // remove the temporary GPU folder when the server exits

	s := &llamaServer{
		cmd:    cmd,
		base:   "http://127.0.0.1:" + strconv.Itoa(port),
		apiKey: key,
		client: &http.Client{Timeout: 5 * time.Minute},
	}
	fmt.Printf("llama-server (%s) starting on 127.0.0.1:%d, log: %s\n", backend, port, logPath)
	if err := s.waitHealthy(ctx, 3*time.Minute); err != nil {
		s.Stop()
		return nil, fmt.Errorf("%w (see %s)", err, logPath)
	}
	return s, nil
}

func (s *llamaServer) Stop() {
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
}

func (s *llamaServer) waitHealthy(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/health", nil)
		resp, err := s.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if s.cmd.ProcessState != nil {
			return errors.New("llama-server exited during startup")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("llama-server did not become healthy in time")
}

// Embed sends one input to POST /v1/embeddings and returns the raw vector.
// Media goes in as typed content parts inside the input item (see docs/decisions.md).
func (s *llamaServer) Embed(ctx context.Context, in spikeInput) ([]float32, error) {
	var item any
	switch in.Kind {
	case "text":
		item = in.Text
	case "image":
		b, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, err
		}
		mime := "image/png"
		if ext := strings.ToLower(filepath.Ext(in.Path)); ext == ".jpg" || ext == ".jpeg" {
			mime = "image/jpeg"
		}
		item = map[string]any{"content": []any{map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)},
		}}}
	case "audio":
		b, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, err
		}
		item = map[string]any{"content": []any{map[string]any{
			"type":        "input_audio",
			"input_audio": map[string]string{"data": base64.StdEncoding.EncodeToString(b), "format": strings.TrimPrefix(strings.ToLower(filepath.Ext(in.Path)), ".")},
		}}}
	default:
		return nil, fmt.Errorf("unknown kind %q", in.Kind)
	}

	body, err := json.Marshal(map[string]any{"model": "embeddinggemma-2", "input": []any{item}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Data) != 1 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("unexpected response: %s", truncate(string(raw), 300))
	}
	for _, x := range parsed.Data[0].Embedding {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, errors.New("embedding contains NaN/Inf")
		}
	}
	return parsed.Data[0].Embedding, nil
}

// truncateNormalize keeps the first n dims (Matryoshka) and L2-normalizes them again.
func truncateNormalize(v []float32, n int) []float32 {
	if len(v) > n {
		v = v[:n]
	}
	out := make([]float32, len(v))
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return out
	}
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// cosine of two already-normalized vectors is just their dot product.
func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
