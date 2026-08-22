package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// 本文件集中存放 fork 定制的配置项，避免在上游 config.go 中插入大段区域。
// 上游 config.go 中仅保留四处单行引用：
//   - Config 结构体中的 Traffic 字段
//   - setDefaults() 中的 setForkDefaults() 调用
//   - load() 中的 cfg.normalizeFork() 调用
//   - Validate() 中的 c.validateFork() 调用

// TrafficConfig controls application-side traffic byte estimation for gateway requests.
type TrafficConfig struct {
	// Enabled: whether to estimate request/response traffic bytes on gateway routes.
	Enabled bool `mapstructure:"enabled"`
	// Source: stored in usage_logs.traffic_source to identify the measurement method.
	Source string `mapstructure:"source"`
	// TLSRecordPayloadBytes: payload bytes per TLS record for overhead estimation.
	TLSRecordPayloadBytes int `mapstructure:"tls_record_payload_bytes"`
	// TLSRecordOverheadBytes: per-record TLS overhead, e.g. TLS 1.3 application data is about 21 bytes.
	TLSRecordOverheadBytes int `mapstructure:"tls_record_overhead_bytes"`
	// TCPIPHeaderBytes: per packet TCP/IP overhead. IPv4 TCP without options is 40 bytes; IPv6 is often 60.
	TCPIPHeaderBytes int `mapstructure:"tcp_ip_header_bytes"`
	// TCPPayloadBytes: estimated TCP payload bytes per packet, normally close to MSS 1460.
	TCPPayloadBytes int `mapstructure:"tcp_payload_bytes"`
}

// setForkDefaults registers fork-only defaults from setDefaults. Keeping this
// explicit is important because tests and embedders may call viper.Reset(),
// which clears defaults registered from package init functions.
func setForkDefaults() {
	// Traffic byte estimation
	viper.SetDefault("traffic.enabled", true)
	viper.SetDefault("traffic.source", "app_estimate")
	viper.SetDefault("traffic.tls_record_payload_bytes", 16*1024)
	viper.SetDefault("traffic.tls_record_overhead_bytes", 21)
	viper.SetDefault("traffic.tcp_ip_header_bytes", 40)
	viper.SetDefault("traffic.tcp_payload_bytes", 1460)
}

// normalizeFork 规整 fork 配置项，在 load() 中调用。
func (c *Config) normalizeFork() {
	c.Traffic.Source = strings.TrimSpace(c.Traffic.Source)
}

// validateFork 校验 fork 配置项，在 Validate() 中调用。
func (c *Config) validateFork() error {
	if c.Traffic.Enabled {
		if c.Traffic.TLSRecordPayloadBytes <= 0 {
			return fmt.Errorf("traffic.tls_record_payload_bytes must be positive")
		}
		if c.Traffic.TLSRecordOverheadBytes < 0 {
			return fmt.Errorf("traffic.tls_record_overhead_bytes must be non-negative")
		}
		if c.Traffic.TCPIPHeaderBytes < 0 {
			return fmt.Errorf("traffic.tcp_ip_header_bytes must be non-negative")
		}
		if c.Traffic.TCPPayloadBytes <= 0 {
			return fmt.Errorf("traffic.tcp_payload_bytes must be positive")
		}
	} else {
		if c.Traffic.TLSRecordPayloadBytes < 0 {
			return fmt.Errorf("traffic.tls_record_payload_bytes must be non-negative")
		}
		if c.Traffic.TLSRecordOverheadBytes < 0 {
			return fmt.Errorf("traffic.tls_record_overhead_bytes must be non-negative")
		}
		if c.Traffic.TCPIPHeaderBytes < 0 {
			return fmt.Errorf("traffic.tcp_ip_header_bytes must be non-negative")
		}
		if c.Traffic.TCPPayloadBytes < 0 {
			return fmt.Errorf("traffic.tcp_payload_bytes must be non-negative")
		}
	}
	return nil
}
