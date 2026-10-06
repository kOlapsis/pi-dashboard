package umami

import "github.com/kolapsis/pi-dashboard/internal/collector"

type Data struct {
	Visits7  int                  `json:"visits7"`
	Prev7    int                  `json:"prev7"`
	DeltaPct *float64             `json:"delta_pct"`
	Live     int                  `json:"live"`
	Bars14   []collector.DayPoint `json:"bars14"`
	Sites    []Site               `json:"sites"`
}

type Site struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Visits7    int                  `json:"visits7"`
	Prev7      int                  `json:"prev7"`
	Pageviews7 int                  `json:"pageviews7"`
	DeltaPct   *float64             `json:"delta_pct"`
	Live       int                  `json:"live"`
	Bars14     []collector.DayPoint `json:"bars14"`
}
