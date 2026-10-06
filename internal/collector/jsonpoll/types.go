package jsonpoll

type Data struct {
	Name  string `json:"name"`
	Items []Item `json:"items"`
}

type Item struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Value  float64  `json:"value"`
	Text   string   `json:"text"`
	Delta7 *float64 `json:"delta7,omitempty"`
}
