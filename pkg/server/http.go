// API to interact with the cache and its stored chunks
package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"nydus-cluster-cache/pkg/cache"
	"nydus-cluster-cache/pkg/nydus"
)

// Uniform first part of the routes, e.g. get /chunks/<hash> or put /chunks/<hash>, under which they are registered below
// TODO: This is only parameterized on the server side (in here), but not in the cacheclients that actually send request to these routes in here => fix via shared package var?
const chunksRoutePrefix = "/chunks/"

// Http handler that can handle concurrent requests and routes requests to the different api routes (e.g. get/put/exist chunk, health)
func NewHandler(clusterCache *cache.Cache, options ...Option) http.Handler {
	var settings handlerSettings
	for _, applyOption := range options {
		applyOption(&settings)
	}
	mux := http.NewServeMux()

	// Dispatch chunk routes via function
	mux.HandleFunc(chunksRoutePrefix, chunkHandler(clusterCache, settings))
	// Health route of this server
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Sets up the chunks routes
func chunkHandler(clusterCache *cache.Cache, settings handlerSettings) http.HandlerFunc {

	// Ugly nesting, but needed so the handler can use the cache => mux handleFunc only accepts the writer and request params
	// => Nesting allows us to use the cache within this function without changing the parameter signature
	return func(respWriter http.ResponseWriter, request *http.Request) {

		// Get the hash by removing the chunks part of the request url path
		hash := strings.TrimPrefix(request.URL.Path, chunksRoutePrefix)

		// Hash cant be empty or contain slashes
		if hash == "" || strings.Contains(hash, "/") {
			http.Error(respWriter, "Invalid hash in path => cant be empty nor contain '/'", http.StatusBadRequest)
			return
		}

		switch request.Method {
		case http.MethodGet:
			serveGet(respWriter, clusterCache, hash)
		case http.MethodPut:
			servePut(respWriter, request, clusterCache, hash, settings)
		case http.MethodHead:
			serveHead(respWriter, clusterCache, hash)
		default:
			http.Error(respWriter, "method is not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func serveGet(writer http.ResponseWriter, clusterCache *cache.Cache, hash string) {
	reader, err := clusterCache.Get(hash)

	// No chunk for this hash
	if errors.Is(err, cache.ErrNotFound) {
		http.Error(writer, "requested chunk with given hash not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	// Chunk found for given hash
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, reader)
}

func servePut(writer http.ResponseWriter, request *http.Request, clusterCache *cache.Cache, hash string, settings handlerSettings) {
	defer request.Body.Close() // TODO: look into why linter cries when this line is removed even though its not actually necessary
	if !settings.checkDigests {
		storeChunk(writer, clusterCache, hash, request.Body)
		return
	}

	// The chunk has to be read in full before its hash can be compared with the name it is stored under
	chunkBytes, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if !nydus.MatchesChunkDigest(chunkBytes, hash) {
		http.Error(writer, "chunk bytes do not match the chunk digest", http.StatusBadRequest)
		return
	}
	storeChunk(writer, clusterCache, hash, bytes.NewReader(chunkBytes))
}

// Writes a chunk into the cache and answers the uploader
func storeChunk(writer http.ResponseWriter, clusterCache *cache.Cache, hash string, chunk io.Reader) {
	if err := clusterCache.Put(hash, chunk); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func serveHead(writer http.ResponseWriter, clusterCache *cache.Cache, hash string) {
	if !clusterCache.Exists(hash) {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	writer.WriteHeader(http.StatusOK)
}
