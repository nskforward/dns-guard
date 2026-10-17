package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/miekg/dns"
)

const dohContentType = "application/dns-message"

// dohResolver — клиент к вышестоящему DNS по DNS-over-HTTPS (RFC 8484).
type dohResolver struct {
	url    string // полный URL для запросов (путь может содержать секрет)
	name   string // отображаемое имя без пути: scheme://host[:port]
	client *http.Client
}

func newDoH(rawURL string, bootstrap []string, hosts map[string]string, timeout time.Duration) (*dohResolver, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("empty DoH url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid DoH url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid DoH url %q: scheme and host are required", rawURL)
	}
	// В статистике и логах показываем только origin (scheme://host[:port]),
	// чтобы не раскрывать секретный путь/параметры DoH-эндпоинта.
	name := u.Scheme + "://" + u.Host
	base := bootstrapDialer(bootstrap, timeout)
	// Если хост URL есть в статическом маппинге, подменяем адрес соединения на
	// IP, сохраняя хостнейм в URL для TLS-SNI и проверки сертификата.
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if ip, ok := staticHost(hostOf(addr), hosts); ok {
			if _, port, err := net.SplitHostPort(addr); err == nil {
				addr = net.JoinHostPort(ip, port)
			}
		}
		return base.DialContext(ctx, network, addr)
	}
	tr := &http.Transport{
		DialContext:         dialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &dohResolver{
		url:    rawURL,
		name:   name,
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
		// *url.Error содержит полный URL с секретным путём — убираем его,
		// оставляя только origin, чтобы токен не попадал в статистику и логи.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("doh %s: %w", d.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh %s: unexpected status %d", d.name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dns.MaxMsgSize))
	if err != nil {
		return nil, fmt.Errorf("doh %s: %w", d.name, err)
	}
	out := new(dns.Msg)
	if err := out.Unpack(body); err != nil {
		return nil, fmt.Errorf("doh %s: unpack: %w", d.name, err)
	}
	return out, nil
}

func (d *dohResolver) String() string { return d.name }

// Close закрывает idle-соединения HTTP-клиента DoH.
func (d *dohResolver) Close() error {
	d.client.CloseIdleConnections()
	return nil
}
