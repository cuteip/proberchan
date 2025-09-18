package probedns

import "testing"

func Test_parseServer(t *testing.T) {
	tests := []struct {
		server   string
		protocol Protocol
		host     string
		port     int
		wantErr  bool
	}{
		{
			server:   "udp://192.0.2.1",
			protocol: ProtocolUDP,
			host:     "192.0.2.1",
			port:     53,
			wantErr:  false,
		},
		{
			server:   "tcp://192.0.2.1",
			protocol: ProtocolTCP,
			host:     "192.0.2.1",
			port:     53,
			wantErr:  false,
		},
		{
			server:   "udp://192.0.2.1:10053",
			protocol: ProtocolUDP,
			host:     "192.0.2.1",
			port:     10053,
			wantErr:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.server, func(t *testing.T) {
			gotProtocol, gotHost, gotPort, gotErr := parseServer(tt.server)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("parseServer() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("parseServer() succeeded unexpectedly")
			}
			if gotProtocol != tt.protocol {
				t.Errorf("parseServer() = %v, want %v", gotProtocol, tt.protocol)
			}
			if gotHost != tt.host {
				t.Errorf("parseServer() = %v, want %v", gotHost, tt.host)
			}
			if gotPort != tt.port {
				t.Errorf("parseServer() = %v, want %v", gotPort, tt.port)
			}
		})
	}
}
