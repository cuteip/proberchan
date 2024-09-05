package dnsutil

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/pkg/errors"
)

type Runner struct {
	// リゾルバ接続情報（<IP_Address>:<Port>）
	// 192.0.2.1:53
	resolver string
}

func New(resolver string) *Runner {
	return &Runner{
		resolver: resolver,
	}
}

// qname の A, AAAA を解決して、IPv4, IPv6 アドレスまとめて返す
// qname は `.` で終わる FQDN
func (r *Runner) ResolveIPAddrByQNAME(ctx context.Context, qname string, qtype dns.Type) ([]netip.Addr, error) {
	ansIPAddr := []netip.Addr{}
	client := dns.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(qname, uint16(qtype))
	m.RecursionDesired = true
	ans, _, err := client.ExchangeContext(ctx, m, r.resolver)
	if err != nil { // TODO: SERVFAIL など判定
		return nil, errors.Wrapf(err, "failed to resolve %s", qname)
	}
	if len(ans.Answer) == 0 {
		return nil, errors.Errorf("rr length is 0: %s", qname)
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
	return ansIPAddr, nil
}

func MustQnameSuffixDot(input string) string {
	if !strings.HasSuffix(input, ".") {
		return fmt.Sprintf("%s.", input)
	}
	return input
}
