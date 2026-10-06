package health

type Data struct {
	Up    int    `json:"up"`
	Total int    `json:"total"`
	Sites []Site `json:"sites"`
}

type Site struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Up        bool   `json:"up"`
	Status    int    `json:"status"`
	LatencyMs int    `json:"latency_ms"`
	CertDays  *int   `json:"cert_days"`
	Error     string `json:"error,omitempty"`
}
