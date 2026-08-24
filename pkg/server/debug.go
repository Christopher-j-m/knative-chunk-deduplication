package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"nydus-cluster-cache/pkg/store"
)

// NewChunkListHandler serves the store's current contents as JSON.
// It exists so a benchmark can capture exactly which chunks a cold start
// brought into the cache; it is read-only and unpaginated, sized for corpus
// images of a few thousand chunks rather than production catalogs.
func NewChunkListHandler(lister store.Lister) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "only GET is supported", http.StatusMethodNotAllowed)
			return
		}

		listing := lister.Chunks()

		// captures land in version control, so the order must not wobble
		// between two runs that saw the same chunks
		sort.Slice(listing, func(i, j int) bool { return listing[i].Hash < listing[j].Hash })

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(listing); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}
