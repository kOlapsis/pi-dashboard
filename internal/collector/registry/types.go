package registry

import "github.com/kolapsis/pi-dashboard/internal/collector"

type Data struct {
	Items []Item `json:"items"`
}

type Item struct {
	Name   string               `json:"name"`
	Kind   string               `json:"kind"`
	Total  int64                `json:"total"`
	Delta7 *int64               `json:"delta7"`
	PerDay []collector.DayPoint `json:"per_day,omitempty"`
	Stars  int                  `json:"stars,omitempty"`
}
