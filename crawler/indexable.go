package crawler

import (
	"context"
	"fmt"
	"github.com/ipfs/go-ipfs-api"
	"log"
	"math/rand"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/label"

	t "github.com/ipfs-search/ipfs-search/types"
)

// Indexable consists of args with a Crawler
type Indexable struct {
	*Crawler
	*Args
}

// String returns '<hash>' (<name>)
func (i *Indexable) String() string {
	if i.Name != "" {
		return fmt.Sprintf("'%s' (%s)", i.Hash, i.Name)
	}
	return fmt.Sprintf("'%s' (Unnamed)", i.Hash)
}

// handleShellError handles IPFS shell errors; returns try again bool and original error
func (i *Indexable) handleShellError(ctx context.Context, err error) (bool, error) {
	if _, ok := err.(*shell.Error); ok && (strings.Contains(err.Error(), "proto") ||
		strings.Contains(err.Error(), "unrecognized type") ||
		strings.Contains(err.Error(), "not a valid merkledag node")) {

		// Attempt to index invalid to prevent re-indexing
		i.indexInvalid(ctx, err)

		// Don't try again, return error
		return false, err
	}

	// Different error, attempt handling as URL error
	return i.handleURLError(err)
}

// handleURLError handles HTTP errors graceously, returns try again bool and original error
// TODO: It is the work of the devil and it must go where dark things go.
func (i *Indexable) handleURLError(err error) (bool, error) {
	if uerr, ok := err.(*url.Error); ok {
		if uerr.Timeout() {
			// Fail on timeouts
			return false, err
		}

		if uerr.Temporary() {
			// Retry on other temp errors
			log.Printf("Temporary URL error: %v", uerr)
			return true, nil
		}

		// Somehow, the errors below are not temp errors !?
		switch t := uerr.Err.(type) {
		case *net.OpError:
			if t.Op == "dial" {
				log.Printf("Unknown host %v", t)
				return true, nil

			} else if t.Op == "read" {
				log.Printf("Connection refused %v", t)
				return true, nil
			}

		case syscall.Errno:
			if t == syscall.ECONNREFUSED {
				log.Printf("Connection refused %v", t)
				return true, nil
			}
		}
	}

	return false, err
}

// hashURL returns the IPFS URL for a particular hash
func (i *Indexable) hashURL() string {
	return fmt.Sprintf("/ipfs/%s", i.Hash)
}

// getFileList return list of files and/or type of item (directory/file)
func (i *Indexable) getFileList(ctx context.Context) (list *shell.UnixLsObject, err error) {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.getFileList")
	defer span.End()

	url := i.hashURL()

	tryAgain := true
	for tryAgain {
		list, err = i.Shell.FileList(url)

		tryAgain, err = i.handleShellError(ctx, err)

		if tryAgain {
			log.Printf("Retrying in %s", i.Config.RetryWait)
			time.Sleep(i.Config.RetryWait)
		}
	}

	return
}

// indexInvalid indexes invalid files to prevent indexing again
func (i *Indexable) indexInvalid(ctx context.Context, err error) {
	// Attempt to index panic to prevent re-indexing
	m := t.Metadata{
		"error": err.Error(),
	}

	i.InvalidIndex.Index(ctx, i.Hash, m)
}

// queueList queues any items in a given list/directory
func (i *Indexable) queueList(ctx context.Context, list *shell.UnixLsObject) (err error) {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.queueList")
	defer span.End()

	for _, link := range list.Links {
		dirArgs := &Args{
			Hash:       link.Hash,
			Name:       link.Name,
			Size:       link.Size,
			ParentHash: i.Hash,
		}

		// Generate random lower priority for items in this directory
		// Rationale; directories might have different availability but
		// within a directory, items are likely to have similar availability.
		// We want consumers to get a varied mixture of availability, for
		// consistent overall indexing load.

		priority := uint8(1 + rand.Intn(7))

		switch link.Type {
		case "File":
			// Add file to crawl queue, with lower priority
			err = i.FileQueue.Publish(ctx, dirArgs, priority)
		case "Directory":
			// Add directory to crawl queue, with lower priority
			err = i.HashQueue.Publish(ctx, dirArgs, priority)
		default:
			log.Printf("Type '%s' skipped for %s", link.Type, i)
			i.indexInvalid(ctx, fmt.Errorf("Unknown type: %s", link.Type))
		}
	}

	return
}

// processList processes and indexes a file listing
func (i *Indexable) processList(ctx context.Context, list *shell.UnixLsObject, references t.References) (err error) {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.processList")
	defer span.End()

	now := nowISO()

	switch list.Type {
	case "File":
		// Add to file crawl queue with high priority
		fileArgs := &Args{
			Hash:       i.Hash,
			Name:       i.Name,
			Size:       list.Size,
			ParentHash: i.ParentHash,
		}

		err = i.FileQueue.Publish(ctx, fileArgs, 9)
	case "Directory":
		// Queue indexing of linked items
		err = i.queueList(ctx, list)
		if err != nil {
			return err
		}

		// Index name and size for directory and directory items
		m := t.Metadata{
			"links":      list.Links,
			"size":       list.Size,
			"references": references,
			"first-seen": now,
			"last-seen":  now,
		}

		err = i.DirectoryIndex.Index(ctx, i.Hash, m)
	default:
		log.Printf("Type '%s' skipped for %s", list.Type, i)
	}

	return
}

// processList processes and indexes a single file
func (i *Indexable) processFile(ctx context.Context, references t.References) error {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.processFile")
	defer span.End()

	m := make(t.Metadata)

	if i.Args.Size > 0 {
		if i.Args.Size > i.Config.MetadataMaxSize {
			// Fail hard for really large files, for now
			return fmt.Errorf("%s too large, not extracting metadata", i)
		}

		// Until refactor, temporarily instantiate ReferencedResource here.
		r := t.ReferencedResource{
			&t.Resource{
				Protocol: t.IPFSProtocol,
				ID:       i.Args.Hash,
			},
			references,
		}

		if err := i.Crawler.Extractor.Extract(ctx, r, m); err != nil {
			return err
		}
	}

	now := nowISO()

	// Add previously found references now
	m["size"] = i.Size
	m["references"] = references
	m["first-seen"] = now
	m["last-seen"] = now

	return i.FileIndex.Index(ctx, i.Hash, m)
}

// preCrawl checks for and returns existing item and conditionally updates it
func (i *Indexable) preCrawl(ctx context.Context) (*existingItem, error) {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.CrawlHash")
	defer span.End()

	e, err := i.getExistingItem(ctx)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}

	return e, e.update(ctx)
}

// CrawlHash crawls a particular hash (file or directory)
func (i *Indexable) CrawlHash(ctx context.Context) error {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.CrawlHash", trace.WithAttributes(
		label.String("cid", i.Args.Hash),
	))
	defer span.End()

	existing, err := i.preCrawl(ctx)

	if err != nil {
		log.Printf("Error in preCrawl: %v", err)
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return err
	}

	if !existing.shouldCrawl() {
		log.Printf("Skipping hash %s", i)
		span.AddEvent(ctx, "skip_hash")
		return nil
	}

	log.Printf("Crawling hash %s", i)

	list, err := i.getFileList(ctx)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return err
	}

	err = i.processList(ctx, list, existing.references)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return err
	}

	log.Printf("Finished hash %s", i)

	return nil
}

// CrawlFile crawls a single object, known to be a file
func (i *Indexable) CrawlFile(ctx context.Context) error {
	ctx, span := i.Tracer.Start(ctx, "crawler.indexable.CrawlFile", trace.WithAttributes(
		label.String("cid", i.Args.Hash),
	))
	defer span.End()

	existing, err := i.preCrawl(ctx)

	if err != nil {
		log.Printf("Error in preCrawl: %v", err)
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return err
	}

	if !existing.shouldCrawl() {
		log.Printf("Skipping file %s", i)
		span.AddEvent(ctx, "skip_file")
		return nil
	}

	log.Printf("Crawling file %s", i)

	i.processFile(ctx, existing.references)
	if err != nil {
		return err
	}

	log.Printf("Finished file %s", i)

	return nil
}

// ReferenceFromIndexable generates a new reference for a given indexable
func ReferenceFromIndexable(i *Indexable) *t.Reference {
	return &t.Reference{
		Name:       i.Name,
		ParentHash: i.ParentHash,
	}
}
