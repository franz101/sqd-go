package main

import (
	"os"

	"github.com/franz101/sqd-go/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
