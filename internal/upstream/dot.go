package upstream

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/miekg/dns"
)

// dotResolver — клиент к вышестоящему DNS по DNS-over-TLS.
type dotResolver struct {
	addr     string // исходный адрес (для String)
	dialAddr string // адрес соединения (может быть IP из hosts)
	host     string
	c        *dns.Client
}

func newDoT(addr string, bootstrap []string, hosts map[string]string, timeout time.Duration) (*dotResolver, error) {
	host := hostOf(addr)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if !isIP(host) {
		tlsCfg.ServerName = host
	}
	// Статический маппинг: соединяемся с IP, но SNI/проверка сертификата — по хостнейму.
	dialAddr := addr
	if ip, ok := staticHost(host, hosts); ok {
		if _, port, err := net.SplitHostPort(addr); err == nil {
			dialAddr = net.JoinHostPort(ip, port)
		}
	}
	c := &dns.Client{
		Net:       "tcp-tls",
		TLSConfig: tlsCfg,
		Timeout:   timeout,
		Dialer:    bootstrapDialer(bootstrap, timeout),
	}
	return &dotResolver{addr: addr, dialAddr: dialAddr, host: host, c: c}, nil
}

func (d *dotResolver) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	resp, _, err := d.c.ExchangeContext(ctx, m, d.dialAddr)
	return resp, err
}

func (d *dotResolver) String() string { return "dot://" + d.addr }

// Close реализует Resolver; соединения DoT устанавливаются на каждый запрос.
func (d *dotResolver) Close() error { return nil }
