// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"time"

	"promremotebench/pkg/generators"

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/pkg/errors"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/version"
	"github.com/prometheus/prometheus/prompb"
	"go.uber.org/zap"
)

func writeLoop(
	generator *generators.HostsSimulator,
	scrapeDuration time.Duration,
	progressBy time.Duration,
	newSeriesPercent float64,
	remotePromClient *Client,
	remotePromBatchSize int,
	logger *zap.Logger,
	checker Checker,
) {
	numWorkers := maxNumScrapesActive *
		int(math.Ceil(float64(scrapeDuration)/float64(progressBy)))
	workers := make(chan struct{}, numWorkers)
	for i := 0; i < numWorkers; i++ {
		workers <- struct{}{}
	}

	ticker := time.NewTicker(progressBy)
	defer ticker.Stop()

	for range ticker.C {
		series, err := generator.Generate(progressBy, scrapeDuration,
			newSeriesPercent)
		if err != nil {
			logger.Fatal("error generating load", zap.Error(err))
		}

		select {
		case token := <-workers:
			go func() {
				remoteWrite(series, remotePromClient,
					remotePromBatchSize, logger)
				workers <- token
			}()
		default:
			// Too many active workers
		}
	}
}

func remoteWrite(
	series []*prompb.TimeSeries,
	remotePromClient *Client,
	remotePromBatchSize int,
	logger *zap.Logger,
	checker Checker,
) {
	i := 0
	for ; i < len(series)-remotePromBatchSize; i += remotePromBatchSize {
		remoteWriteBatch(series[i:i+remotePromBatchSize], remotePromClient, logger)
	}

	checker.Write(series[i:])

	remoteWriteBatch(series[i:], remotePromClient, logger)
}

func remoteWriteBatch(
	series []*prompb.TimeSeries,
	remotePromClient *Client,
	logger *zap.Logger,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := &prompb.WriteRequest{
		Timeseries: series,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		logger.Error("error marshalling prompb write request", zap.Error(err))
		return
	}

	encoded := snappy.Encode(nil, data)
	err = remotePromClient.Store(ctx, encoded)
	if err != nil {
		logger.Error("error writing to remote prom store", zap.Error(err))
		return
	}
}

const maxErrMsgLen = 256

var userAgent = fmt.Sprintf("Prometheus/%s", version.Version)

// Client allows reading and writing from/to a remote HTTP endpoint.
type Client struct {
	index   int // Used to differentiate clients in metrics.
	url     string
	client  *http.Client
	timeout time.Duration
}

type recoverableError struct {
	error
}

// NewClient creates a new Client.
func NewClient(url string, timeout time.Duration) (*Client, error) {
	httpClient, err := config.NewClientFromConfig(config.HTTPClientConfig{}, "remote_storage")
	if err != nil {
		return nil, err
	}

	return &Client{
		url:     url,
		client:  httpClient,
		timeout: time.Duration(timeout),
	}, nil
}

// Store sends a batch of samples to the HTTP endpoint, the request is the proto marshalled
// and encoded bytes from codec.go.
func (c *Client) Store(ctx context.Context, req []byte) error {
	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(req))
	if err != nil {
		// Errors from NewRequest are from unparseable URLs, so are not
		// recoverable.
		return err
	}
	httpReq.Header.Add("Content-Encoding", "snappy")
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	httpReq = httpReq.WithContext(ctx)

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	httpResp, err := c.client.Do(httpReq.WithContext(ctx))
	if err != nil {
		// Errors from client.Do are from (for example) network errors, so are
		// recoverable.
		return recoverableError{err}
	}
	defer func() {
		io.Copy(ioutil.Discard, httpResp.Body)
		httpResp.Body.Close()
	}()

	if httpResp.StatusCode/100 != 2 {
		scanner := bufio.NewScanner(io.LimitReader(httpResp.Body, maxErrMsgLen))
		line := ""
		if scanner.Scan() {
			line = scanner.Text()
		}
		err = errors.Errorf("server returned HTTP status %s: %s", httpResp.Status, line)
	}
	if httpResp.StatusCode/100 == 5 {
		return recoverableError{err}
	}
	return err
}
