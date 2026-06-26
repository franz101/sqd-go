package ingestion

import "github.com/franz101/sqd-go/internal/client"

func clientNew(endpoint string) *client.Client { return client.New(endpoint) }
