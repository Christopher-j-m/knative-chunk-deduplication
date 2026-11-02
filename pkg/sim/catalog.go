package sim

import (
	"encoding/json"
	"fmt"
	"os"
)

// Load all the container images, change each of their hashes with a short number (for faster comparison)
// that will also be used for the bit position in bitset

// Catalog of all container images with their short number, corresponding chunks and size
type Catalog struct {
	Images     []string         // Container images
	Sizes      []uint64         // Each chunks size (bytes)
	Sets       [][]uint32       // Each unique chunk gets a short number assigned => Contains the chunk number that the corresponding image contains
	ImageBytes []uint64         // image size (bytes) of image via sum of chunk sizes
	index      map[string]int32 // lookup image name -> image number
}

// Format of the input json file for an extracted image and its corresponding chunks
// Will be used to load these json files in LoadCatalog() func
type imageChunksFile struct {
	Image  string `json:"image"`
	Chunks []struct {
		Hash string `json:"hash"`
		Size uint64 `json:"size"`
	} `json:"chunks"`
}

// Read the image data json file that need to be created via /scripts/gen-churn-catalog.py
// For each image it assigns each chunk, it hasnt seen in other container images already, a number
// If it has already seen a specific chunk, it just references this chunk by the previously, already assigned number
func LoadCatalog(imageFilePath string) (*Catalog, error) {
	imageFileData, err := os.ReadFile(imageFilePath)
	if err != nil {
		return nil, fmt.Errorf("error while reading image data file: %w", err)
	}

	var images []imageChunksFile
	if err := json.Unmarshal(imageFileData, &images); err != nil {
		return nil, fmt.Errorf("error while parsing catalog: %w", err)
	}

	imageCatalog := &Catalog{index: map[string]int32{}}
	ids := map[string]uint32{}
	for _, currImage := range images {
		if _, dup := imageCatalog.index[currImage.Image]; dup {
			return nil, fmt.Errorf("image %s found twice", currImage.Image)
		}

		// Assign the image the next available number (counting up)
		imageCatalog.index[currImage.Image] = int32(len(imageCatalog.Images))
		imageCatalog.Images = append(imageCatalog.Images, currImage.Image)

		seen := map[uint32]bool{}
		var set []uint32
		var bytes uint64
		for _, currChunk := range currImage.Chunks {
			id, ok := ids[currChunk.Hash]

			// Not seen yet => Assign next available number
			if !ok {
				id = uint32(len(imageCatalog.Sizes))
				ids[currChunk.Hash] = id
				imageCatalog.Sizes = append(imageCatalog.Sizes, currChunk.Size)
			}

			// We already seen this chunk and assigned a number to it
			if seen[id] {
				continue
			}

			seen[id] = true
			set = append(set, id)
			bytes += imageCatalog.Sizes[id]
		}

		imageCatalog.Sets = append(imageCatalog.Sets, set)
		imageCatalog.ImageBytes = append(imageCatalog.ImageBytes, bytes)
	}

	return imageCatalog, nil
}

// Returns the position in the catalog image by name (and if its contained or not)
func (imageCatalog *Catalog) Index(image string) (int32, bool) {
	index, ok := imageCatalog.index[image]
	return index, ok
}

// Counts for each chunk in how many images it is contained
// => refcount eviction strat needs this
func (imageCatalog *Catalog) RefCounts() []int {
	refs := make([]int, len(imageCatalog.Sizes))

	// For each image, look into each of its chunks and increment their individual counters
	for _, set := range imageCatalog.Sets {
		for _, id := range set {
			refs[id]++
		}
	}

	return refs
}
