package calendar

import "time"

type Data struct {
	Today    []Event `json:"today"`
	Upcoming []Event `json:"upcoming"`
}

type Event struct {
	Title      string    `json:"title"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	AllDay     bool      `json:"all_day"`
	InProgress bool      `json:"in_progress"`
	Calendar   string    `json:"calendar"`
	Location   string    `json:"location,omitempty"`
}
