package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// fetcher performs the requests of one refresh.
type fetcher struct {
	ctx    context.Context
	client *http.Client
}

// get reads one URL and refuses a response larger than bound. A body
// beyond the bound is an error, never a truncated read: a partial
// definition is not a definition.
func (f fetcher) get(rawurl, accept string, bound int) ([]byte, error) {
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, &FetchError{Err: err}
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, &FetchError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &FetchError{Err: fmt.Errorf("%s: %s", rawurl, resp.Status)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(bound)+1))
	if err != nil {
		return nil, &FetchError{Err: err}
	}
	if len(data) > bound {
		return nil, &FetchError{Err: fmt.Errorf("%s: the response exceeds %d bytes", rawurl, bound)}
	}
	return data, nil
}

// httpClient returns the client to use. An injected client keeps its own
// transport and timeout, but not its redirect policy: the policy is a
// property of this package, so a source cannot escape it through a
// caller-supplied client.
func (f fetcher) httpClient() *http.Client {
	if f.client == nil {
		return DefaultClient()
	}
	c := *f.client
	c.CheckRedirect = redirectPolicy
	return &c
}

// errTooLarge reports a snapshot beyond the total bound.
func errTooLarge(location string) error {
	return fmt.Errorf("the snapshot of %s exceeds %d bytes", location, MaxSnapshotBytes)
}
