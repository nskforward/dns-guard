package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

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

// applyECSConfig применяет настройки ECS: обновляет статические подсети и
// (пере)запускает фоновое автоопределение публичного адреса.
func (s *Server) applyECSConfig(cfg *config.Config) {
	v4, v6 := cfg.ECSNets()
	gen := s.ecsGen.Add(1)

	s.ecsMu.Lock()
	if s.ecsCancel != nil {
		s.ecsCancel()
		s.ecsCancel = nil
	}
	s.ecsStatic = ecsNets{v4: v4, v6: v6}
	s.ecsNets = s.ecsStatic
	s.ecsMu.Unlock()

	for _, n := range []*net.IPNet{v4, v6} {
		if n == nil {
			continue
		}
		if n.IP.IsPrivate() || n.IP.IsLoopback() || n.IP.IsUnspecified() {
			s.log.Warn("ecs subnet is not public; authoritative servers cannot geolocate it", "subnet", n.String())
		}
	}

	ecsCfg := cfg.ECS
	if !ecsCfg.Enabled {
		return
	}
	ad := ecsCfg.Autodetect
	if ad.URL == "" && ad.URL6 == "" {
		if v4 == nil && v6 == nil {
			s.log.Warn("ecs enabled but no subnet configured")
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ecsMu.Lock()
	s.ecsCancel = cancel
	s.ecsMu.Unlock()
	if v4 == nil && v6 == nil {
		s.log.Info("ecs autodetect started", "interval", ecsCfg.AutodetectInterval().String())
	}
	go s.detectLoop(ctx, ecsCfg, gen)
}

// detectLoop периодически обновляет публичную подсеть.
func (s *Server) detectLoop(ctx context.Context, cfg config.ECS, gen uint64) {
	s.detectOnce(ctx, cfg, gen)
	ticker := time.NewTicker(cfg.AutodetectInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.detectOnce(ctx, cfg, gen)
		}
	}
}

// detectOnce определяет публичный адрес по HTTP и обновляет подсети ECS.
// При ошибке сохраняется последнее известное значение (статическое или ранее
// определённое), поэтому сбой сервиса автодетекта не ломает резолвинг.
func (s *Server) detectOnce(ctx context.Context, cfg config.ECS, gen uint64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var detected ecsNets
	if cfg.Autodetect.URL != "" {
		ip, err := fetchPublicIP(ctx, cfg.Autodetect.URL)
		switch {
		case err != nil:
			s.log.Warn("ecs autodetect failed", "url", cfg.Autodetect.URL, "err", err)
		case ip.To4() == nil:
			s.log.Warn("ecs autodetect: expected IPv4", "url", cfg.Autodetect.URL, "ip", ip.String())
		default:
			detected.v4 = ipNet(ip.To4(), 24)
		}
	}
	if cfg.Autodetect.URL6 != "" {
		ip, err := fetchPublicIP(ctx, cfg.Autodetect.URL6)
		switch {
		case err != nil:
			s.log.Warn("ecs autodetect failed", "url", cfg.Autodetect.URL6, "err", err)
		case ip.To4() != nil:
			s.log.Warn("ecs autodetect: expected IPv6", "url", cfg.Autodetect.URL6, "ip", ip.String())
		default:
			detected.v6 = ipNet(ip.To16(), 56)
		}
	}
	if detected.v4 == nil && detected.v6 == nil {
		return
	}
	// Пока результат устарел (конфиг перезагружен) — не применяем.
	if s.ecsGen.Load() != gen {
		return
	}

	s.ecsMu.Lock()
	nets := ecsNets{v4: detected.v4, v6: detected.v6}
	if nets.v4 == nil {
		nets.v4 = s.ecsStatic.v4
	}
	if nets.v6 == nil {
		nets.v6 = s.ecsStatic.v6
	}
	changed := !sameNet(nets.v4, s.ecsNets.v4) || !sameNet(nets.v6, s.ecsNets.v6)
	if changed {
		s.ecsNets = nets
	}
	s.ecsMu.Unlock()

	if !changed {
		return
	}
	if s.cache != nil {
		// Ответы, полученные с прежней подсетью, неверны для новой.
		s.cache.Flush()
	}
	s.log.Info("ecs subnet updated", "v4", netString(nets.v4), "v6", netString(nets.v6))
}

// fetchPublicIP запрашивает у HTTP-сервиса публичный адрес в текстовом виде.
func fetchPublicIP(ctx context.Context, url string) (net.IP, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(b))
	ip := net.ParseIP(text)
	if ip == nil {
		return nil, fmt.Errorf("response is not an IP address: %q", text)
	}
	return ip, nil
}

// ipNet строит нормализованную сеть из адреса и длины префикса.
func ipNet(ip net.IP, bits int) *net.IPNet {
	mask := net.CIDRMask(bits, len(ip)*8)
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}
}

func sameNet(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.String() == b.String()
}

func netString(n *net.IPNet) string {
	if n == nil {
		return ""
	}
	return n.String()
}
