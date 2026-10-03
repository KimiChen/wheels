package monitor

// This DTO is only attached to the administrative snapshot, never to the
// public projection or telemetry. It carries no native Dashboard credentials.
type nativeAccess struct {
	ServerDashboardURL  string              `json:"server_dashboard_url"`
	ClientDashboardURLs map[string]string   `json:"client_dashboard_urls"`
	PublishedEndpoints  []publishedEndpoint `json:"published_endpoints"`
}

type publishedEndpoint struct {
	ProxyName string `json:"proxy_name"`
	User      string `json:"user"`
	ClientID  string `json:"client_id"`
	URL       string `json:"url"`
}

func (s *Service) nativeAccessSnapshot() nativeAccess {
	out := nativeAccess{ClientDashboardURLs: map[string]string{}, PublishedEndpoints: []publishedEndpoint{}}
	cfg := s.cfg.NativeAccess
	if cfg == nil {
		return out
	}
	out.ServerDashboardURL = cfg.ServerDashboardURL
	for id, link := range cfg.ClientDashboardURLs {
		out.ClientDashboardURLs[id] = link
	}
	for _, entry := range cfg.PublishedEndpoints {
		out.PublishedEndpoints = append(out.PublishedEndpoints, publishedEndpoint{ProxyName: entry.ProxyName, User: entry.User, ClientID: entry.ClientID, URL: entry.URL})
	}
	return out
}
