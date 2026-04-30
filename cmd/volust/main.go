package main

import (
	"fmt"
	"os"

	"github.com/monlor/volust/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
