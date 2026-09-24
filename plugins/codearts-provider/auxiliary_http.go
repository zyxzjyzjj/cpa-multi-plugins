package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// CPA's current C ABI has no per-call deadline on host.http.do. Only initial
// optional catalogue discovery uses this small, cancellable client; chats and
// management requests continue through the host transport. Never detach I/O.
type auxiliaryDoFunc func(context.Context, *Config, string, string, map[string]string, []byte) (*hostHTTPResponse, error)

var auxiliaryDoSlot atomic.Value

func auxiliaryHTTP(ctx context.Context, cfg *Config, method, endpoint string, headers map[string]string, body []byte) (*hostHTTPResponse, error) {
	if fn, _ := auxiliaryDoSlot.Load().(auxiliaryDoFunc); fn != nil {
		return fn(ctx, cfg, method, endpoint, headers, body)
	}
	return auxiliaryHTTPDirect(ctx, cfg, method, endpoint, headers, body)
}

func auxiliaryHTTPDirect(ctx context.Context, cfg *Config, method, endpoint string, headers map[string]string, body []byte) (*hostHTTPResponse, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.EqualFold(cfg.DiscoveryProxyURL, "direct://") {
		transport.Proxy = nil
	} else if cfg.DiscoveryProxyURL != "" {
		proxy, err := url.Parse(cfg.DiscoveryProxyURL)
		if err != nil || proxy.Host == "" {
			return nil, fmt.Errorf("invalid discovery_proxy_url")
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid discovery endpoint")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("benefit discovery timed out or failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return nil, fmt.Errorf("invalid benefit discovery response size")
	}
	return &hostHTTPResponse{StatusCode: resp.StatusCode, Body: b}, nil
}
