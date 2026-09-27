package shared

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const Version = "0.4.0-p4"

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
	ProbeEnabled          bool   `json:"probeEnabled,omitempty"`
	ProbeAllowPrivate     bool   `json:"probeAllowPrivate,omitempty"`
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
	Enabled                bool     `json:"enabled,omitempty"`
	BindAddr               string   `json:"bindAddr,omitempty"`
	BindPort               int      `json:"bindPort,omitempty"`
	ServerID               string   `json:"serverID,omitempty"`
	CertFile               string   `json:"certFile,omitempty"`
	KeyFile                string   `json:"keyFile,omitempty"`
	ReportIntervalSeconds  int      `json:"reportIntervalSeconds,omitempty"`
	DatabaseFile           string   `json:"databaseFile,omitempty"`
	RetentionDays          int      `json:"retentionDays,omitempty"`
	HistoryDataPath        string   `json:"historyDataPath,omitempty"`
	GitHubClientID         string   `json:"githubClientID,omitempty"`
	GitHubClientSecretFile string   `json:"githubClientSecretFile,omitempty"`
	GitHubCallbackURL      string   `json:"githubCallbackURL,omitempty"`
	GitHubAdminUsers       []string `json:"githubAdminUsers,omitempty"`
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
	if c.RetentionDays == 0 {
		c.RetentionDays = 7
	}
	return c.Validate()
}

func (c MonitorConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	ip := net.ParseIP(c.BindAddr)
	if ip == nil || c.BindPort < 1 || c.BindPort > 65535 {
		return errors.New("invalid monitor bind address or port")
	}
	if !bounded(c.ServerID, 128, false) || c.ReportIntervalSeconds < 1 || c.ReportIntervalSeconds > 3600 {
		return errors.New("invalid monitor serverID or reportIntervalSeconds")
	}
	if !configPath(c.DatabaseFile, true) || !configPath(c.CertFile, false) || !configPath(c.KeyFile, false) {
		return errors.New("invalid monitor credential or TLS file path")
	}
	if !configPath(c.HistoryDataPath, c.HistoryDataPath != "") || !configPath(c.GitHubClientSecretFile, false) {
		return errors.New("invalid monitor history or GitHub secret path")
	}
	if c.HistoryDataPath != "" && (c.RetentionDays < 1 || c.RetentionDays > 365) {
		return errors.New("monitor retentionDays must be 1..365")
	}
	configured := c.GitHubClientID != "" || c.GitHubClientSecretFile != "" || c.GitHubCallbackURL != "" || len(c.GitHubAdminUsers) > 0
	if configured {
		if !bounded(c.GitHubClientID, 256, false) || !configPath(c.GitHubClientSecretFile, true) || len(c.GitHubAdminUsers) == 0 || len(c.GitHubAdminUsers) > 32 {
			return errors.New("GitHub client, secret file, callback and administrator usernames are required together")
		}
		if len(c.GitHubCallbackURL) > 4096 {
			return errors.New("GitHub callback URL is too long")
		}
		u, err := url.Parse(c.GitHubCallbackURL)
		if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path != "/api/admin/v1/auth/github/callback" {
			return errors.New("invalid GitHub callback URL")
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("invalid GitHub callback port")
			}
		}
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
			return errors.New("GitHub callback requires HTTPS or literal loopback HTTP")
		}
		for _, login := range c.GitHubAdminUsers {
			if len(login) < 1 || len(login) > 39 || strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") {
				return errors.New("invalid GitHub administrator username")
			}
			for _, ch := range login {
				if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
					return errors.New("invalid GitHub administrator username")
				}
			}
		}
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
