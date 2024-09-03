package dnsutil

import (
	"context"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

type Runner struct {
	// リゾルバ接続情報（<IP_Address>:<Port>）
	// 192.0.2.1:53
	resolver string

	queryQtypes []uint16
}

func New(resolver string, disableIPv4 bool, disableIPv6 bool) *Runner {
	qtype := []uint16{}
	if !disableIPv4 {
		qtype = append(qtype, dns.TypeA)
	}
	if !disableIPv6 {
		qtype = append(qtype, dns.TypeAAAA)
	}
	return &Runner{
		resolver:    resolver,
		queryQtypes: qtype,
	}
}

// qname の A, AAAA を解決して、IPv4, IPv6 アドレスまとめて返す
// qname は `.` で終わる FQDN
func (r *Runner) ResolveIPAddrByQNAME(ctx context.Context, qname string) ([]netip.Addr, error) {
	ansIPAddr := []netip.Addr{}
	client := dns.Client{Net: "udp", Timeout: 2 * time.Second}
	for _, qtype := range r.queryQtypes {
		m := new(dns.Msg)
		m.SetQuestion(qname, qtype)
		m.RecursionDesired = true
		ans, _, err := client.ExchangeContext(ctx, m, r.resolver)
		if err != nil { // TODO: SERVFAIL など判定
			return nil, err
		}
		for _, a := range ans.Answer {
			switch answer := a.(type) {
			case *dns.A:
				ipAddr, ok := netip.AddrFromSlice(answer.A)
				if !ok {
					continue // 無視
				}
				ansIPAddr = append(ansIPAddr, ipAddr)
			case *dns.AAAA:
				ipAddr, ok := netip.AddrFromSlice(answer.AAAA)
				if !ok {
					continue // 無視
				}
				ansIPAddr = append(ansIPAddr, ipAddr)
			}
		}
	}
	return ansIPAddr, nil
}
