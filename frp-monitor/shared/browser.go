package shared

import "strconv"

// BrowserFacts and BrowserMetrics are precision-safe transport shapes. They are
// NOT public-view DTOs: the monitor must still authorize and redact private fields.
type BrowserFacts struct {
	Scope        Scope         `json:"scope"`
	Hostname     Field[string] `json:"hostname"`
	OS           Field[string] `json:"os"`
	Kernel       Field[string] `json:"kernel"`
	Arch         Field[string] `json:"arch"`
	Virt         Field[string] `json:"virt"`
	CPUName      Field[string] `json:"cpu_name"`
	CPUCores     Field[uint32] `json:"cpu_cores"`
	AgentVersion Field[string] `json:"agent_version"`
	IPv4         Field[string] `json:"ipv4"`
	IPv6         Field[string] `json:"ipv6"`
	MemTotal     Field[string] `json:"mem_total"`
	SwapTotal    Field[string] `json:"swap_total"`
	DiskTotal    Field[string] `json:"disk_total"`
}

type BrowserMetrics struct {
	Scope      Scope            `json:"scope"`
	CPU        Field[float64]   `json:"cpu"`
	Load       Field[[]float64] `json:"load"`
	MemTotal   Field[string]    `json:"mem_total"`
	MemUsed    Field[string]    `json:"mem_used"`
	SwapTotal  Field[string]    `json:"swap_total"`
	SwapUsed   Field[string]    `json:"swap_used"`
	DiskTotal  Field[string]    `json:"disk_total"`
	DiskUsed   Field[string]    `json:"disk_used"`
	NetRX      Field[string]    `json:"net_rx"`
	NetTX      Field[string]    `json:"net_tx"`
	NetRXTotal Field[string]    `json:"net_rx_total"`
	NetTXTotal Field[string]    `json:"net_tx_total"`
	BootID     Field[string]    `json:"boot_id"`
	Iface      Field[string]    `json:"iface"`
	Uptime     Field[string]    `json:"uptime"`
	TCP        Field[string]    `json:"tcp"`
	UDP        Field[string]    `json:"udp"`
	Procs      Field[string]    `json:"procs"`
}

func decimal(f Field[uint64]) Field[string] {
	out := Field[string]{Quality: f.Quality, Reason: f.Reason}
	if f.Value != nil {
		v := strconv.FormatUint(*f.Value, 10)
		out.Value = &v
	}
	return out
}

// Browser validates first, then converts every uint64 observation to decimal text.
func (f Facts) Browser() (BrowserFacts, error) {
	if err := f.Validate(); err != nil {
		return BrowserFacts{}, err
	}
	return BrowserFacts{
		Scope: f.Scope, Hostname: f.Hostname, OS: f.OS, Kernel: f.Kernel, Arch: f.Arch, Virt: f.Virt,
		CPUName: f.CPUName, CPUCores: f.CPUCores, AgentVersion: f.AgentVersion, IPv4: f.IPv4, IPv6: f.IPv6,
		MemTotal: decimal(f.MemTotal), SwapTotal: decimal(f.SwapTotal), DiskTotal: decimal(f.DiskTotal),
	}, nil
}
func (m Metrics) Browser() (BrowserMetrics, error) {
	if err := m.Validate(); err != nil {
		return BrowserMetrics{}, err
	}
	return BrowserMetrics{
		Scope: m.Scope, CPU: m.CPU, Load: m.Load, MemTotal: decimal(m.MemTotal), MemUsed: decimal(m.MemUsed),
		SwapTotal: decimal(m.SwapTotal), SwapUsed: decimal(m.SwapUsed), DiskTotal: decimal(m.DiskTotal), DiskUsed: decimal(m.DiskUsed),
		NetRX: decimal(m.NetRX), NetTX: decimal(m.NetTX), NetRXTotal: decimal(m.NetRXTotal), NetTXTotal: decimal(m.NetTXTotal),
		BootID: m.BootID, Iface: m.Iface, Uptime: decimal(m.Uptime), TCP: decimal(m.TCP), UDP: decimal(m.UDP), Procs: decimal(m.Procs),
	}, nil
}
