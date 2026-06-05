package jtypes

// easyjson:json
type JSONLBlock struct {
	Header JSONLHeader `json:"header"`
	Logs   []JSONLLog  `json:"logs"`
}

// easyjson:json
type JSONLHeader struct {
	Number    uint64 `json:"number"`
	Hash      string `json:"hash"`
	Timestamp uint64 `json:"timestamp"`
}

// easyjson:json
type JSONLLog struct {
	Address          string   `json:"address,nocopy"`
	TransactionHash  string   `json:"transactionHash,nocopy"`
	Data             string   `json:"data,nocopy"`
	Topics           []string `json:"topics,nocopy"`
	TransactionIndex uint64   `json:"transactionIndex"`
	LogIndex         uint64   `json:"logIndex"`
}
