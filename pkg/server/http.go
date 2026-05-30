package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"nydus-cluster-cache/pkg/cache"
)

const chunksPrefix = "/chunks/"

// NewHandler wires HTTP routes for the given cache. The returned handler is
// safe for concurrent use and does not own the cache lifecycle.
func NewHandler(c *cache.Cache) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(chunksPrefix, chunkHandler(c))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func chunkHandler(c *cache.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := strings.TrimPrefix(r.URL.Path, chunksPrefix)
		if hash == "" || strings.Contains(hash, "/") {
			http.Error(w, "invalid chunk hash", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodGet:
			serveGet(w, c, hash)
		case http.MethodPut:
			servePut(w, r, c, hash)
		case http.MethodHead:
			serveHead(w, c, hash)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func serveGet(w http.ResponseWriter, c *cache.Cache, hash string) {
	reader, err := c.Get(hash)
	if errors.Is(err, cache.ErrNotFound) {
		http.Error(w, "chunk not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

func servePut(w http.ResponseWriter, r *http.Request, c *cache.Cache, hash string) {
	defer r.Body.Close()
	if err := c.Put(hash, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serveHead(w http.ResponseWriter, c *cache.Cache, hash string) {
	if !c.Exists(hash) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}
