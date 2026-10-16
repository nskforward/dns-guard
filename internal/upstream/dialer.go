package upstream

import (
	"context"
	"net"
	"strings"
	"time"
)

// bootstrapDialer возвращает net.Dialer, который резолвит имена upstream-сервера
// через заданные bootstrap-DNS (plain UDP). Если bootstrap не задан — используется
// системный резолвер.
func bootstrapDialer(bootstrap []string, timeout time.Duration) *net.Dialer {
	d := &net.Dialer{Timeout: timeout}
	if len(bootstrap) == 0 {
		return d
	}
	servers := make([]string, 0, len(bootstrap))
	for _, b := range bootstrap {
		servers = append(servers, normalizeHostPort(b, 53))
	}
	d.Resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var lastErr error
			for _, s := range servers {
				c, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, s)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return d
}

// hostOf выделяет хостнейм из "host:port".
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// isIP сообщает, является ли строка IP-адресом.
func isIP(s string) bool {
	return net.ParseIP(strings.Trim(s, "[]")) != nil
}
