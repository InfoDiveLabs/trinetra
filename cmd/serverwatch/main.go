package main

import (
	"os"

	"serverwatch/internal/serverwatch"
)

func main() { os.Exit(serverwatch.Main(os.Args[1:])) }
