package maintenant

type Data struct {
	Instances []Instance `json:"instances"`
}

type Instance struct {
	Name              string  `json:"name"`
	Status            string  `json:"status"`
	Message           string  `json:"message,omitempty"`
	Incidents         int     `json:"incidents"`
	ComponentsDown    int     `json:"components_down"`
	ComponentsTotal   int     `json:"components_total"`
	Alerts            *Alerts `json:"alerts,omitempty"`
	Hosts             *int    `json:"hosts,omitempty"`
	ContainersRunning *int    `json:"containers_running,omitempty"`
	ContainersTotal   *int    `json:"containers_total,omitempty"`
	Error             string  `json:"error,omitempty"`
}

type Alerts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}
