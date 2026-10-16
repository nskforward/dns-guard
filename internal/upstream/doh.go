package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/miekg/dns"
)

const dohContentType = "application/dns-message"

// dohResolver — клиент к вышестоящему DNS по DNS-over-HTTPS (RFC 8484).
type dohResolver struct {
	url    string
	client *http.Client
}

func newDoH(url string, bootstrap []string, timeout time.Duration) (*dohResolver, error) {
	if url == "" {
		return nil, fmt.Errorf("empty DoH url")
	}
	dialer := bootstrapDialer(bootstrap, timeout)
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &dohResolver{
		url:    url,
		client: &http.Client{Transport: tr, Timeout: timeout},
	}, nil
}

func (d *dohResolver) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	packed, err := m.Pack()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dohContentType)
	req.Header.Set("Accept", dohContentType)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dns.MaxMsgSize))
	if err != nil {
		return nil, err
	}
	out := new(dns.Msg)
	if err := out.Unpack(body); err != nil {
		return nil, fmt.Errorf("doh: unpack: %w", err)
	}
	return out, nil
}

func (d *dohResolver) String() string { return d.url }

// Close закрывает idle-соединения HTTP-клиента DoH.
func (d *dohResolver) Close() error {
	d.client.CloseIdleConnections()
	return nil
}
