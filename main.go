package main

import (
	"os"

	"github.com/franz101/sqd-go-v2/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
