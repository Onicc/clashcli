package main

import (
	"fmt"
	"os"

	"github.com/Onicc/clashcli/internal/app"
)

var version = "dev"

func main() {
	if err := app.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", app.Redact(err.Error()))
		os.Exit(1)
	}
}
