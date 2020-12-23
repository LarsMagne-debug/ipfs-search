package crawler

import (
	"github.com/ipfs-search/ipfs-search/queue"
)

// Queues used for crawling.
type Queues struct {
	Files       queue.Publisher
	Directories queue.Publisher
	Hashes      queue.Publisher
}
