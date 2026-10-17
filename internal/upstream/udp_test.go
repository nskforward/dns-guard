package upstream

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// TestUDPFallsBackToTCPOnTruncation проверяет, что усечённый по UDP ответ
// автоматически перезапрашивается по TCP и клиенту уходит полный ответ.
func TestUDPFallsBackToTCPOnTruncation(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	addr := pc.LocalAddr().String()

	// 40 A-записей гарантированно не влезают в 512 байт.
	full := new(dns.Msg)
	full.SetQuestion("example.com.", dns.TypeA)
	full.Response = true
	for i := 0; i < 40; i++ {
		rr, err := dns.NewRR("example.com. 60 IN A 10.0.0.1")
		if err != nil {
			t.Fatalf("new rr: %v", err)
		}
		full.Answer = append(full.Answer, rr)
	}

	h := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := full.Copy()
		m.SetReply(r)
		if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
			m.Truncate(dns.MinMsgSize)
		}
		_ = w.WriteMsg(m)
	})

	udpSrv := &dns.Server{PacketConn: pc, Handler: h}
	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	tcpSrv := &dns.Server{Listener: tcpLn, Handler: h}

	go func() { _ = udpSrv.ActivateAndServe() }()
	go func() { _ = tcpSrv.ActivateAndServe() }()
	defer func() {
		_ = udpSrv.Shutdown()
		_ = tcpSrv.Shutdown()
	}()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)

	r := newUDP(addr, time.Second)
	resp, err := r.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Truncated {
		t.Fatal("response must be fetched over TCP, not truncated")
	}
	if len(resp.Answer) != len(full.Answer) {
		t.Fatalf("answers = %d, want %d", len(resp.Answer), len(full.Answer))
	}
}
