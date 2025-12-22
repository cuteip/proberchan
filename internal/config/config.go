package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DNSResolver string        `yaml:"dns_resolver"`
	Probes      []ProbeConfig `yaml:"probes"`
}

type ProbeConfig struct {
	Name string      `yaml:"name"`
	Type string      `yaml:"type"`
	Ping *PingConfig `yaml:"ping,omitempty"`
	HTTP *HTTPConfig `yaml:"http,omitempty"`
	DNS  *DNSConfig  `yaml:"dns,omitempty"`
}

type PingTarget struct {
	Host        string `yaml:"host"`
	Description string `yaml:"description,omitempty"`
}

type PingConfig struct {
	Targets           []PingTarget `yaml:"targets"`
	IntervalMs        int          `yaml:"interval_ms"`
	TimeoutMs         int          `yaml:"timeout_ms"`
	DF                bool         `yaml:"df"`
	Size              int          `yaml:"size"`
	Src               string       `yaml:"src"`
	ResolveIPVersions []int        `yaml:"resolve_ip_versions"`
}

type HTTPTarget struct {
	URL         string `yaml:"url"`
	Description string `yaml:"description,omitempty"`
}

type HTTPConfig struct {
	Targets           []HTTPTarget `yaml:"targets"`
	IntervalMs        int          `yaml:"interval_ms"`
	TimeoutMs         int          `yaml:"timeout_ms"`
	ResolveIPVersions []int        `yaml:"resolve_ip_versions"`
	UserAgent         string       `yaml:"user_agent"`
}

type DNSConfig struct {
	Targets           []DNSTarget `yaml:"targets"`
	IntervalMs        int         `yaml:"interval_ms"`
	TimeoutMs         int         `yaml:"timeout_ms"`
	ResolveIPVersions []int       `yaml:"resolve_ip_versions"`
	FlagRD            bool        `yaml:"flag_rd"`
}
type DNSTarget struct {
	Server      string `yaml:"server"`
	QName       string `yaml:"qname"`
	QType       string `yaml:"qtype"`
	Description string `yaml:"description,omitempty"`
}

func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config: %w", err)
	}

	return &config, nil
}

func (c *Config) Validate() error {
	if c.DNSResolver == "" {
		return fmt.Errorf("dns_resolver is required")
	}

	if len(c.Probes) == 0 {
		return fmt.Errorf("at least one probe is required")
	}

	for i, probe := range c.Probes {
		if err := probe.Validate(); err != nil {
			return fmt.Errorf("probe %d (%s): %w", i, probe.Name, err)
		}
	}

	return nil
}

func (c *Config) GetProbe(name string) (*ProbeConfig, bool) {
	for _, probe := range c.Probes {
		if probe.Name == name {
			return &probe, true
		}
	}
	return nil, false
}

func (p *ProbeConfig) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("probe name is required")
	}

	switch p.Type {
	case "ping":
		if p.Ping == nil {
			return fmt.Errorf("ping configuration is required for ping probe")
		}
		return p.Ping.Validate()
	case "http":
		if p.HTTP == nil {
			return fmt.Errorf("http configuration is required for http probe")
		}
		return p.HTTP.Validate()
	case "dns":
		if p.DNS == nil {
			return fmt.Errorf("dns configuration is required for dns probe")
		}
		return p.DNS.Validate()
	default:
		return fmt.Errorf("unsupported probe type: %s", p.Type)
	}
}

func (c *PingConfig) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	if len(c.ResolveIPVersions) == 0 {
		return fmt.Errorf("at least one resolve_ip_version is required")
	}
	for _, version := range c.ResolveIPVersions {
		if version != 4 && version != 6 {
			return fmt.Errorf("invalid IP version: %d (must be 4 or 6)", version)
		}
	}
	return nil
}

func (c *HTTPConfig) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	if len(c.ResolveIPVersions) == 0 {
		return fmt.Errorf("at least one resolve_ip_version is required")
	}
	for _, version := range c.ResolveIPVersions {
		if version != 4 && version != 6 {
			return fmt.Errorf("invalid IP version: %d (must be 4 or 6)", version)
		}
	}
	return nil
}

func (c *DNSConfig) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	if len(c.ResolveIPVersions) == 0 {
		return fmt.Errorf("at least one resolve_ip_version is required")
	}
	for _, version := range c.ResolveIPVersions {
		if version != 4 && version != 6 {
			return fmt.Errorf("invalid IP version: %d (must be 4 or 6)", version)
		}
	}
	return nil
}
