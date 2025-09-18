package probedns

import "fmt"

type Protocol string

const (
	ProtocolUDP Protocol = "udp"
	ProtocolTCP Protocol = "tcp"
)

func NewProtocol(p string) (Protocol, error) {
	switch p {
	case "udp":
		return ProtocolUDP, nil
	case "tcp":
		return ProtocolTCP, nil
	default:
		return "", fmt.Errorf("unknown protocol: %s", p)
	}
}

func (p Protocol) String() string {
	return string(p)
}
