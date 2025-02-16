// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/tools/querytee/proxy_endpoint.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querytee

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	util_log "github.com/grafana/mimir/pkg/util/log"
)

type ResponsesComparator interface {
	Compare(expected, actual []byte) error
}

type ProxyEndpoint struct {
	backends   []*ProxyBackend
	metrics    *ProxyMetrics
	logger     log.Logger
	comparator ResponsesComparator

	// Whether for this endpoint there's a preferred backend configured.
	hasPreferredBackend bool

	// The route name used to track metrics.
	routeName string
}

func NewProxyEndpoint(backends []*ProxyBackend, routeName string, metrics *ProxyMetrics, logger log.Logger, comparator ResponsesComparator) *ProxyEndpoint {
	hasPreferredBackend := false
	for _, backend := range backends {
		if backend.preferred {
			hasPreferredBackend = true
			break
		}
	}

	return &ProxyEndpoint{
		backends:            backends,
		routeName:           routeName,
		metrics:             metrics,
		logger:              logger,
		comparator:          comparator,
		hasPreferredBackend: hasPreferredBackend,
	}
}

func (p *ProxyEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Send the same request to all backends.
	resCh := make(chan *backendResponse, len(p.backends))
	go p.executeBackendRequests(r, resCh)

	// Wait for the first response that's feasible to be sent back to the client.
	downstreamRes := p.waitBackendResponseForDownstream(resCh)

	if downstreamRes.err != nil {
		http.Error(w, downstreamRes.err.Error(), http.StatusInternalServerError)
	} else {
		w.WriteHeader(downstreamRes.status)
		if _, err := w.Write(downstreamRes.body); err != nil {
			level.Warn(p.logger).Log("msg", "Unable to write response", "err", err)
		}
	}

	p.metrics.responsesTotal.WithLabelValues(downstreamRes.backend.name, r.Method, p.routeName).Inc()
}

func (p *ProxyEndpoint) executeBackendRequests(r *http.Request, resCh chan *backendResponse) {
	var (
		wg           = sync.WaitGroup{}
		err          error
		body         []byte
		responses    = make([]*backendResponse, 0, len(p.backends))
		responsesMtx = sync.Mutex{}
		query        = r.URL.RawQuery
	)

	if r.Body != nil {
		body, err = ioutil.ReadAll(r.Body)
		if err != nil {
			level.Warn(p.logger).Log("msg", "Unable to read request body", "err", err)
			return
		}
		if err := r.Body.Close(); err != nil {
			level.Warn(p.logger).Log("msg", "Unable to close request body", "err", err)
		}

		r.Body = ioutil.NopCloser(bytes.NewReader(body))
		if err := r.ParseForm(); err != nil {
			level.Warn(p.logger).Log("msg", "Unable to parse form", "err", err)
		}
		query = r.Form.Encode()
	}

	level.Debug(p.logger).Log("msg", "Received request", "path", r.URL.Path, "query", query)

	wg.Add(len(p.backends))
	for _, b := range p.backends {
		b := b

		go func() {
			defer wg.Done()
			var (
				bodyReader io.ReadCloser
				start      = time.Now()
			)
			if len(body) > 0 {
				bodyReader = ioutil.NopCloser(bytes.NewReader(body))
			}

			status, body, err := b.ForwardRequest(r, bodyReader)
			elapsed := time.Since(start)

			res := &backendResponse{
				backend: b,
				status:  status,
				body:    body,
				err:     err,
			}

			// Log with a level based on the backend response.
			lvl := level.Debug
			if !res.succeeded() {
				lvl = level.Warn
			}

			lvl(p.logger).Log("msg", "Backend response", "path", r.URL.Path, "query", query, "backend", b.name, "status", status, "elapsed", elapsed)
			p.metrics.requestDuration.WithLabelValues(res.backend.name, r.Method, p.routeName, strconv.Itoa(res.statusCode())).Observe(elapsed.Seconds())

			// Keep track of the response if required.
			if p.comparator != nil {
				responsesMtx.Lock()
				responses = append(responses, res)
				responsesMtx.Unlock()
			}

			resCh <- res
		}()
	}

	// Wait until all backend requests completed.
	wg.Wait()
	close(resCh)

	// Compare responses.
	if p.comparator != nil {
		expectedResponse := responses[0]
		actualResponse := responses[1]
		if responses[1].backend.preferred {
			expectedResponse, actualResponse = actualResponse, expectedResponse
		}

		result := comparisonSuccess
		err := p.compareResponses(expectedResponse, actualResponse)
		if err != nil {
			level.Error(util_log.Logger).Log("msg", "response comparison failed", "route-name", p.routeName,
				"query", r.URL.RawQuery, "err", err)
			result = comparisonFailed
		}

		p.metrics.responsesComparedTotal.WithLabelValues(p.routeName, result).Inc()
	}
}

func (p *ProxyEndpoint) waitBackendResponseForDownstream(resCh chan *backendResponse) *backendResponse {
	var (
		responses                 = make([]*backendResponse, 0, len(p.backends))
		preferredResponseReceived = false
	)

	for res := range resCh {
		// If the response is successful we can immediately return it if:
		// - There's no preferred backend configured
		// - Or this response is from the preferred backend
		// - Or the preferred backend response has already been received and wasn't successful
		if res.succeeded() && (!p.hasPreferredBackend || res.backend.preferred || preferredResponseReceived) {
			return res
		}

		// If we received a non-successful response from the preferred backend, then we can
		// return the first successful response received so far (if any).
		if res.backend.preferred && !res.succeeded() {
			preferredResponseReceived = true

			for _, prevRes := range responses {
				if prevRes.succeeded() {
					return prevRes
				}
			}
		}

		// Otherwise we keep track of it for later.
		responses = append(responses, res)
	}

	// No successful response, so let's pick the first one.
	return responses[0]
}

func (p *ProxyEndpoint) compareResponses(expectedResponse, actualResponse *backendResponse) error {
	// compare response body only if we get a 200
	if expectedResponse.status != 200 {
		return fmt.Errorf("skipped comparison of response because we got status code %d from preferred backend's response", expectedResponse.status)
	}

	if actualResponse.status != 200 {
		return fmt.Errorf("skipped comparison of response because we got status code %d from secondary backend's response", actualResponse.status)
	}

	if expectedResponse.status != actualResponse.status {
		return fmt.Errorf("expected status code %d but got %d", expectedResponse.status, actualResponse.status)
	}

	return p.comparator.Compare(expectedResponse.body, actualResponse.body)
}

type backendResponse struct {
	backend *ProxyBackend
	status  int
	body    []byte
	err     error
}

func (r *backendResponse) succeeded() bool {
	if r.err != nil {
		return false
	}

	// We consider the response successful if it's a 2xx or 4xx (but not 429).
	return (r.status >= 200 && r.status < 300) || (r.status >= 400 && r.status < 500 && r.status != 429)
}

func (r *backendResponse) statusCode() int {
	if r.err != nil || r.status <= 0 {
		return 500
	}

	return r.status
}
