package weather

import "time"

type Data struct {
	Label    string    `json:"label"`
	Temp     float64   `json:"temp"`
	Feels    float64   `json:"feels"`
	Code     int       `json:"code"`
	IsDay    bool      `json:"is_day"`
	TMin     float64   `json:"tmin"`
	TMax     float64   `json:"tmax"`
	RainPct  int       `json:"rain_pct"`
	Sunrise  time.Time `json:"sunrise"`
	Sunset   time.Time `json:"sunset"`
	Tomorrow *Day      `json:"tomorrow,omitempty"`
}

type Day struct {
	Code    int     `json:"code"`
	TMin    float64 `json:"tmin"`
	TMax    float64 `json:"tmax"`
	RainPct int     `json:"rain_pct"`
}
