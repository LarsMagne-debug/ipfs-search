package crawler

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"log"
	"math/rand"

	indexTypes "github.com/ipfs-search/ipfs-search/index/types"
	t "github.com/ipfs-search/ipfs-search/types"
)

var (
	// ErrDirectoryTooLarge is returned by Ls() when a directory is larger `Config.MaxDirSize`.
	ErrDirectoryTooLarge = t.WrappedError{Err: t.ErrInvalidResource, Msg: "directory too large"}

	// errEndOfLs is an internal error to communicate the end of hte list from processNextDirEntry to processDirEntries.
	errEndOfLs = errors.New("end of list")
)

func (c *Crawler) crawlDir(ctx context.Context, r *t.AnnotatedResource, properties *indexTypes.Directory) error {
	entries := make(chan *t.AnnotatedResource, c.config.DirEntryBufferSize)

	wg, ctx := errgroup.WithContext(ctx)

	wg.Go(func() error {
		return c.processDirEntries(ctx, entries, properties)
	})

	wg.Go(func() error {
		defer close(entries)
		return c.protocol.Ls(ctx, r, entries)
	})

	return wg.Wait()
}

func resourceToLinkType(r *t.AnnotatedResource) (indexTypes.LinkType, error) {
	switch r.Type {
	case t.FileType:
		return indexTypes.FileLinkType, nil
	case t.DirectoryType:
		return indexTypes.DirectoryLinkType, nil
	case t.UndefinedType:
		return indexTypes.UnknownLinkType, nil
	case t.UnsupportedType:
		return indexTypes.UnsupportedLinkType, nil
	default:
		// TODO: Test this
		return "", fmt.Errorf("%w: %v", ErrUnexpectedType, r.Type)
	}
}

func addLink(entry *t.AnnotatedResource, properties *indexTypes.Directory) error {
	linkType, err := resourceToLinkType(entry)
	if err != nil {
		return err
	}

	properties.Links = append(properties.Links, indexTypes.Link{
		Hash: entry.ID,
		Name: entry.Reference.Name,
		Size: entry.Size,
		Type: linkType,
	})

	return nil
}

func (c *Crawler) processDirEntries(ctx context.Context, entries <-chan *t.AnnotatedResource, properties *indexTypes.Directory) error {
	var dirCnt uint = 0

	// Question: do we need a maximum entry cutoff point? E.g. 10^6 entries or something?
	processNextDirEntry := func() error {
		// Create (and cancel!) a new timeout context for every entry.
		ctx, cancel := context.WithTimeout(ctx, c.config.DirEntryTimeout)
		defer cancel()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case entry, ok := <-entries:
			if !ok {
				return errEndOfLs
			}

			if dirCnt%1024 == 0 {
				log.Printf("Processed %d directory entries in %v.", dirCnt, entry.Parent)
				log.Printf("Latest entry: %v", entry)
			}

			// Only add to properties up to limit (preventing oversized directory entries) - but queue nonetheless.
			if dirCnt <= c.config.MaxDirSize {
				// TODO: Ensure coverage.
				if err := addLink(entry, properties); err != nil {
					return err
				}
			}

			// TODO: Consider doing this in a separate Goroutine, as it's blocking.
			return c.queueDirEntry(ctx, entry)
		}
	}

	var err error

	// Process entries until error.
	for err == nil {
		err = processNextDirEntry()
		dirCnt++
	}

	if errors.Is(err, errEndOfLs) {
		// End of list; we're done

		if dirCnt == c.config.MaxDirSize {
			// TODO: Ensure coverage.
			return ErrDirectoryTooLarge
		}

		return nil
	}

	// Fail hard on error; prefer less over incomplete or inconsistent data.
	return err
}

func (c *Crawler) queueDirEntry(ctx context.Context, r *t.AnnotatedResource) error {
	// Generate random lower priority for items in this directory
	// Rationale; directories might have different availability but
	// within a directory, items are likely to have similar availability.
	// We want consumers to get a varied mixture of availability, for
	// consistent overall indexing load.
	priority := uint8(1 + rand.Intn(7))

	switch r.Type {
	case t.UndefinedType:
		return c.queues.Hashes.Publish(ctx, r, priority)
	case t.FileType:
		return c.queues.Files.Publish(ctx, r, priority)
	case t.DirectoryType:
		return c.queues.Directories.Publish(ctx, r, priority)
	case t.UnsupportedType:
		// Index right away as invalid.
		// Rationale: as no additional protocol request is required and queue'ing returns
		// similarly fast as indexing.
		return c.indexInvalid(ctx, r, t.ErrUnsupportedType)
	default:
		return fmt.Errorf("%w: %v", ErrUnexpectedType, r.Type)
	}
}
