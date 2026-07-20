package main

import (
	"fmt"
	"os"

	"ccw/internal/config"
)

func main() {
	if _, err := config.Load(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("cclaude: config ok")
}
