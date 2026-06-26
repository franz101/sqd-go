package main
import (
	"os"

	"github.com/franz101/sqd-go/internal/cli"
	_ "github.com/franz101/sqd-go/examples/uniswap/src"
)


func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
