package main

import (
	"os"

	_ "github.com/franz101/sqd-go/examples/uniswap/generated"
	"github.com/franz101/sqd-go/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
