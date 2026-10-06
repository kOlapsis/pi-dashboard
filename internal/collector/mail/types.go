package mail

import "time"

type Data struct {
	Account string `json:"account"`
	Unseen  int    `json:"unseen"`
	Items   []Item `json:"items"`
}

type Item struct {
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}
