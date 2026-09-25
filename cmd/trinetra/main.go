package main

import (
	"os"

	"github.com/InfoDiveLabs/trinetra/internal/trinetra"
)

func main() { os.Exit(trinetra.Main(os.Args[1:])) }
