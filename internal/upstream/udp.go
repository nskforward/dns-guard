package upstream

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// udpResolver — клиент к вышестоящему DNS по UDP/53.
type udpResolver struct {
	addr string
	c    *dns.Client
	tcp  *dns.Client
}

func newUDP(addr string, timeout time.Duration) *udpResolver {
	return &udpResolver{
		addr: addr,
		c: &dns.Client{
			Net:     "udp",
			Timeout: timeout,
			UDPSize: 4096,
		},
		tcp: &dns.Client{
			Net:     "tcp",
			Timeout: timeout,
		},
	}
}

func (u *udpResolver) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	resp, _, err := u.c.ExchangeContext(ctx, m, u.addr)
	if err != nil {
		return resp, err
	}
	if resp == nil || !resp.Truncated {
		return resp, nil
	}
	// Усечённый ответ бесполезен клиенту (и не кэшируется), поэтому повторяем
	// запрос по TCP к тому же серверу. Если TCP недоступен, возвращаем
	// исходный усечённый ответ — не хуже, чем было.
	if full, _, tcpErr := u.tcp.ExchangeContext(ctx, m, u.addr); tcpErr == nil && full != nil {
		return full, nil
	}
	return resp, nil
}

func (u *udpResolver) String() string { return "udp://" + u.addr }

// Close реализует Resolver; UDP-клиент не держит постоянных соединений.
func (u *udpResolver) Close() error { return nil }

// normalizeHostPort добавляет порт по умолчанию, если он не указан.
func normalizeHostPort(addr string, defPort int) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(defPort))
}
