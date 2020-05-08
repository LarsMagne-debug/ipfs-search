package sniffer

import (
	"context"
	"errors"
	t "github.com/ipfs-search/ipfs-search/types"
	"time"
)

type logger interface {
	Next() (map[string]interface{}, error)
}

type providerExtractor interface {
	Extract(map[string]interface{}) (*t.Provider, error)
}

// ErrorLoggerTimeout represents a timeout from the IPFS shell's logger.
var ErrorLoggerTimeout = errors.New("Timeout waiting for log messages")

// The default IPFS logger is a blocking function without a context, hence
// we wrap it in a goroutine to allow for timeouts.
// TODO: Upgrade to well-designed `go-ipfs-http-api` if and when Logger is
// implemented there and/or to use the generic `Request()` from there.
func loggerToChannel(ctx context.Context, l logger, msgs chan<- map[string]interface{}, errc chan<- error) {
	for {
		select {
		case <-ctx.Done():
			errc <- ctx.Err()
			return
		default:
			msg, err := l.Next()
			if err != nil {
				errc <- err
			}

			msgs <- msg
		}
	}
}

func getProviders(ctx context.Context, l logger, e providerExtractor, providers chan<- t.Provider, timeout time.Duration) error {
	msgs := make(chan map[string]interface{})
	errc := make(chan error, 1)

	go loggerToChannel(ctx, l, msgs, errc)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(timeout):
			return ErrorLoggerTimeout
		case err := <-errc:
			return err
		case msg := <-msgs:
			provider, err := e.Extract(msg)
			if err != nil {
				return err
			}

			if provider != nil {
				providers <- *provider
			}
		}
	}
}
