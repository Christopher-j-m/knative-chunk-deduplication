package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"nydus-cluster-cache/pkg/store"
)

// Debug endpoint (only get/head /debug/chunks) that lists all chunks in our cache => sorted by hash so we dont need to hassle with random ordering
// Also used for measuring which chunks are pulled into the cache during cold starts => sorting helps avoiding duplicate data on re-runs to be commited (no git diff in that case)
func NewChunkListHandler(lister store.Lister) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			http.Error(writer, "only Get/Head is supported", http.StatusMethodNotAllowed)
			return
		}

		listing := lister.Chunks()

		// Sort chunks lexicographic
		sort.Slice(listing, func(i, j int) bool { return listing[i].Hash < listing[j].Hash })

		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(listing); err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
		}
	})
}
