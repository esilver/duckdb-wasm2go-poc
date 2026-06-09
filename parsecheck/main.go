//go:build ignore
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"time"
)

func main() {
	t := time.Now()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, os.Args[1], nil, parser.SkipObjectResolution)
	if err != nil {
		fmt.Println("PARSE ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("PARSED OK: package %s, %d top-level decls, in %s\n", f.Name.Name, len(f.Decls), time.Since(t))
}
