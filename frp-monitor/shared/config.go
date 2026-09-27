package shared

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const Version = "0.1.0-p1"

// AgentConfig is embedded in the native frpc configuration as [telemetry].
// Runtime secrets are loaded from private files, never from these public types.
type AgentConfig struct {
	Enabled               bool   `json:"enabled,omitempty"`
	Endpoint              string `json:"endpoint,omitempty"`
	TokenFile             string `json:"tokenFile,omitempty"`
	CAFile                string `json:"caFile,omitempty"`
	ServerID              string `json:"serverID,omitempty"`
	Iface                 string `json:"iface,omitempty"`
	IntervalSeconds       int    `json:"intervalSeconds,omitempty"`
	AllowInsecureLoopback bool   `json:"allowInsecureLoopback,omitempty"`
}

func (c *AgentConfig) Complete() error {
	if !c.Enabled {
		return nil
	}
	if c.IntervalSeconds == 0 {
		c.IntervalSeconds = 1
	}
	if c.ServerID == "" {
		c.ServerID = "default"
	}
	return c.Validate()
}

func (c AgentConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.IntervalSeconds < 1 || c.IntervalSeconds > 3600 {
		return errors.New("telemetry intervalSeconds must be 1..3600")
	}
	if !bounded(c.ServerID, 128, false) || !configPath(c.TokenFile, true) || !configPath(c.CAFile, false) {
		return errors.New("invalid telemetry serverID or credential/CA file path")
	}
	if len(c.Endpoint) > 4096 {
		return errors.New("telemetry endpoint is too long")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "/agent/v1/ws" || u.RawPath != "" {
		return errors.New("telemetry endpoint must identify /agent/v1/ws without userinfo, query or fragment")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid telemetry endpoint port")
		}
	}
	if u.Scheme != "wss" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "ws" || !c.AllowInsecureLoopback || ip == nil || !ip.IsLoopback() {
			return errors.New("telemetry requires wss; explicit development ws is restricted to literal loopback")
		}
	}
	if !bounded(c.Iface, MaxStringBytes, true) {
		return errors.New("invalid telemetry iface")
	}
	return nil
}

// MonitorConfig is embedded in the native frps configuration as [monitor].
type MonitorConfig struct {
	Enabled               bool   `json:"enabled,omitempty"`
	BindAddr              string `json:"bindAddr,omitempty"`
	BindPort              int    `json:"bindPort,omitempty"`
	ServerID              string `json:"serverID,omitempty"`
	CertFile              string `json:"certFile,omitempty"`
	KeyFile               string `json:"keyFile,omitempty"`
	CredentialsFile       string `json:"credentialsFile,omitempty"`
	ReportIntervalSeconds int    `json:"reportIntervalSeconds,omitempty"`
}

func (c *MonitorConfig) Complete() error {
	if !c.Enabled {
		return nil
	}
	if c.BindAddr == "" {
		c.BindAddr = "127.0.0.1"
	}
	if c.BindPort == 0 {
		c.BindPort = 7401
	}
	if c.ServerID == "" {
		c.ServerID = "default"
	}
	if c.ReportIntervalSeconds == 0 {
		c.ReportIntervalSeconds = 1
	}
	return c.Validate()
}

func (c MonitorConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	ip := net.ParseIP(c.BindAddr)
	if ip == nil || c.BindPort < 0 || c.BindPort > 65535 {
		return errors.New("invalid monitor bind address or port")
	}
	if !bounded(c.ServerID, 128, false) || c.ReportIntervalSeconds < 1 || c.ReportIntervalSeconds > 3600 {
		return errors.New("invalid monitor serverID or reportIntervalSeconds")
	}
	if !configPath(c.CredentialsFile, true) || !configPath(c.CertFile, false) || !configPath(c.KeyFile, false) {
		return errors.New("invalid monitor credential or TLS file path")
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("monitor certFile and keyFile must be specified together")
	}
	if !ip.IsLoopback() && c.CertFile == "" {
		return errors.New("non-loopback monitor listener requires TLS")
	}
	return nil
}

func configPath(s string, required bool) bool {
	return (!required || strings.TrimSpace(s) != "") && len(s) <= 4096 && !strings.ContainsAny(s, "\x00\r\n")
}
