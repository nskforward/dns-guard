package server

import (
	"net"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/config"
)

// ecsNets — набор публичных подсетей, передаваемых в EDNS Client Subnet.
type ecsNets struct {
	v4 *net.IPNet
	v6 *net.IPNet
}

// forQtype выбирает подсеть по типу запроса: A — IPv4, AAAA — IPv6 (с откатом
// на IPv4), для остальных типов предпочтительна IPv4. Пустая сеть → nil.
func (n ecsNets) forQtype(qtype uint16) *net.IPNet {
	if qtype == dns.TypeAAAA {
		if n.v6 != nil {
			return n.v6
		}
		return n.v4
	}
	if n.v4 != nil {
		return n.v4
	}
	return n.v6
}

// setECS добавляет к исходящему запросу EDNS Client Subnet (RFC 7871).
// Клиентская ECS-опция, если она была, заменяется нашей: доверять подсети,
// присланной клиентом, нельзя. Остальные EDNS-опции и DO-бит сохраняются.
func setECS(m *dns.Msg, nets ecsNets, qtype uint16) {
	ipnet := nets.forQtype(qtype)
	if ipnet == nil {
		return
	}
	opt := m.IsEdns0()
	if opt == nil {
		m.SetEdns0(1232, false)
		if opt = m.IsEdns0(); opt == nil {
			return
		}
	}

	// Убираем прежние ECS-опции (клиентские или от предыдущего вызова).
	kept := opt.Option[:0]
	for _, o := range opt.Option {
		if o.Option() != dns.EDNS0SUBNET {
			kept = append(kept, o)
		}
	}
	opt.Option = kept

	family := uint16(1)
	addr := ipnet.IP.To4()
	if addr == nil {
		family = 2
		addr = ipnet.IP.To16()
	}
	if addr == nil {
		return
	}
	ones, _ := ipnet.Mask.Size()
	opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        family,
		SourceNetmask: uint8(ones),
		SourceScope:   0,
		Address:       addr,
	})
}

// fitEDNS приводит OPT ответа к возможностям клиента: если клиент не присылал
// EDNS, OPT (добавленный нами ради ECS в запросе к апстриму) удаляется.
func fitEDNS(resp, req *dns.Msg) {
	if resp == nil || req == nil || req.IsEdns0() != nil {
		return
	}
	if len(resp.Extra) == 0 {
		return
	}
	extra := resp.Extra[:0]
	for _, rr := range resp.Extra {
		if _, ok := rr.(*dns.OPT); ok {
			continue
		}
		extra = append(extra, rr)
	}
	resp.Extra = extra
}

// applyECS добавляет ECS в исходящий запрос с учётом текущих подсетей.
func (s *Server) applyECS(m *dns.Msg, qtype uint16) {
	s.ecsMu.RLock()
	nets := s.ecsNets
	s.ecsMu.RUnlock()
	setECS(m, nets, qtype)
}

// applyECSConfig применяет статические подсети из конфигурации.
func (s *Server) applyECSConfig(cfg *config.Config) {
	v4, v6 := cfg.ECSNets()

	s.ecsMu.Lock()
	s.ecsNets = ecsNets{v4: v4, v6: v6}
	s.ecsMu.Unlock()

	for _, n := range []*net.IPNet{v4, v6} {
		if n == nil {
			continue
		}
		if n.IP.IsPrivate() || n.IP.IsLoopback() || n.IP.IsUnspecified() {
			s.log.Warn("ecs subnet is not public; authoritative servers cannot geolocate it", "subnet", n.String())
		}
	}
	if cfg.ECS.Enabled && v4 == nil && v6 == nil {
		s.log.Warn("ecs enabled but no subnet configured")
	}
}
