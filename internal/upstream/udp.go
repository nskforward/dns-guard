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
}

func newUDP(addr string, timeout time.Duration) *udpResolver {
	return &udpResolver{
		addr: addr,
		c: &dns.Client{
			Net:     "udp",
			Timeout: timeout,
			UDPSize: 4096,
		},
	}
}

func (u *udpResolver) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	resp, _, err := u.c.ExchangeContext(ctx, m, u.addr)
	return resp, err
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
