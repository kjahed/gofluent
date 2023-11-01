package main

import (
	"flag"
	"strings"

	"github.com/kjahed/gofluent/gen"
)

var (
	inPkgsFlag    = flag.String("pkgs", "", "Go packages containing the structs to generate the API for")
	outputDirFlag = flag.String("out", "", "Output dir/pkg for the generated API")
)

func main() {
	flag.Parse()
	if *inPkgsFlag == "" {
		panic("missing required -pkgs arg\n")
	}
	if *outputDirFlag == "" {
		panic("missing required -out arg\n")
	}

	gc := &gen.GeneratorConfig{
		Pkgs:   strings.Split(*inPkgsFlag, ","),
		OutDir: *outputDirFlag,
	}

	if err := gen.Generate(gc); err != nil {
		panic(err)
	}
}
