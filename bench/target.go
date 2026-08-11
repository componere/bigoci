package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/imgoci/bigoci"
)

// newTargetClient builds the library client one cell talks through, plus
// the counter that watches its traffic.
//
// The counting transport goes in through WithHTTPClient, which the library
// documents as the caller's outermost transport, so the counter sees every
// request the transfer makes — token exchanges and redirect hops included.
func newTargetClient(target Target) (*bigoci.Client, *statusCounter, error) {
	counter := &statusCounter{}

	opts := []bigoci.Option{
		bigoci.WithHTTPClient(&http.Client{Transport: counter}),
	}
	if target.PlainHTTP {
		opts = append(opts, bigoci.WithPlainHTTP())
	}
	if target.AuthEnv != "" {
		username, token, err := credentialsFromEnv(target.AuthEnv)
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, bigoci.WithCredentials(username, token))
	}

	client, err := bigoci.New(opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("build client for target %q: %w", target.Name, err)
	}

	return client, counter, nil
}

// credentialsFromEnv reads the <name>_USERNAME / <name>_TOKEN pair a
// target's auth_env names. Both must be set: a half-configured credential
// should stop the run before it burns server time on auth failures.
func credentialsFromEnv(name string) (string, string, error) {
	username := os.Getenv(name + "_USERNAME")
	token := os.Getenv(name + "_TOKEN")
	if username == "" || token == "" {
		return "", "", fmt.Errorf(
			"target auth_env %q: set both %s_USERNAME and %s_TOKEN in the environment", name, name, name,
		)
	}

	return username, token, nil
}

// statusCounter is a transport wrapper that counts responses outside the
// 2xx and 3xx families, keyed by status code.
//
// It exists for the design's open question: whether worker count should
// self-tune on 429 and 503. A result row that carries real throttle counts
// from a real registry turns that decision into data. The counter is reset
// at the start of each timed phase, so every row's counts belong to that
// row alone.
type statusCounter struct {
	// mu guards counts; transfer workers share one transport.
	mu sync.Mutex
	// counts holds occurrences per status code.
	counts map[int]int
}

// RoundTrip forwards the request to the default transport and counts the
// response if its status signals trouble.
func (c *statusCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		// Returned unwrapped on purpose: a transport must surface the exact
		// error shape net/http produced, or the library's retry
		// classification loses the types it matches on.
		return resp, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		c.mu.Lock()
		if c.counts == nil {
			c.counts = make(map[int]int)
		}
		c.counts[resp.StatusCode]++
		c.mu.Unlock()
	}

	return resp, nil
}

// reset clears the counts at the start of a timed phase.
func (c *statusCounter) reset() {
	c.mu.Lock()
	c.counts = nil
	c.mu.Unlock()
}

// snapshot returns the counts accumulated since the last reset, keyed by
// the status code's decimal string — the shape the result row marshals.
// A nil map means a clean phase, which JSON omits entirely.
func (c *statusCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.counts) == 0 {
		return nil
	}
	statuses := make(map[string]int, len(c.counts))
	for code, count := range c.counts {
		statuses[strconv.Itoa(code)] = count
	}

	return statuses
}
