// Command fakeserver imitates llama-server for tests: it serves /health and
// /v1/embeddings on --port (127.0.0.1), checks the API key from LLAMA_API_KEY, and exits
// at once when an input contains "poison" (to simulate a crash on a bad input).
package main

import (
	"encoding/json"
	"flag"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func main() {
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	port := fs.Int("port", 0, "")
	// Accept and ignore every other llama-server flag.
	var args []string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--port" && i+1 < len(os.Args) {
			args = append(args, "-port", os.Args[i+1])
		}
	}
	fs.Parse(args)
	key := os.Getenv("LLAMA_API_KEY")

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	http.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, in := range req.Input {
			if strings.Contains(in, "poison") {
				os.Exit(3) // crash while handling the request
			}
			v := make([]float32, 768)
			for j := range v {
				v[j] = float32(math.Sin(float64(len(in)*7 + j)))
			}
			data = append(data, item{i, v})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	http.ListenAndServe("127.0.0.1:"+strconv.Itoa(*port), nil)
}
