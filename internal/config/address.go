package config

import (
	"fmt"
	"strings"
)

// protocols — набор поддерживаемых протоколов апстримов.
var protocols = []string{"udp", "dot", "doh"}

// SplitProtocol отделяет необязательный префикс протокола "<udp|dot|doh>+"
// от адреса апстрима. Префикс перекрывает протокол группы, что позволяет
// собирать группу из серверов разных протоколов — например, UDP primary и
// DoH fallback:
//
//	udp+192.168.1.1:53
//	doh+https://9.9.9.9/dns-query
//
// Если префикса нет, возвращаются proto == "" и исходный адрес.
//
// Префиксом считается только ведущий токен без разделителей "/:?" — это
// оставляет URL вида "https://host/dns-query?a=1+2" нетронутыми.
func SplitProtocol(addr string) (proto, rest string, err error) {
	i := strings.IndexByte(addr, '+')
	if i <= 0 {
		return "", addr, nil
	}
	token := strings.TrimSpace(addr[:i])
	if strings.ContainsAny(token, "/:?") {
		return "", addr, nil
	}
	lower := strings.ToLower(token)
	for _, p := range protocols {
		if lower == p {
			return p, strings.TrimSpace(addr[i+1:]), nil
		}
	}
	return "", "", fmt.Errorf("unknown protocol prefix %q in upstream address %q", token, addr)
}
