package main

import (
	"fmt"
	"os"

	"incus-for-devs/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "incus-for-devs:", err)
		os.Exit(1)
	}
}
