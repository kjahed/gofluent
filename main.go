package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"

	"golang.org/x/tools/go/packages"
)

var (
	inPkg        = flag.String("inPkg", "", "Go package to generate API for")
	targetStruct = flag.String("structName", "", "Go struct to generate API for")
	outputFile   = flag.String("outFile", "", "Output Go file for the generated API")
	outputPkg    = flag.String("outPkg", "", "Output package name for the generated files")
)

type (
	typeAttr struct {
		Name         string
		IsPtr        bool
		IsSlice      bool
		IsMap        bool
		IsStruct     bool
		SliceValType *typeAttr
		MapKeyType   *typeAttr
		MapValType   *typeAttr
		StructAttr   *structAttr
	}

	fieldAttr struct {
		FieldName string
		ValType   *typeAttr
	}

	structAttr struct {
		Name   string
		Fields []*fieldAttr
	}
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
	os.Exit(1)
}

func prettyPrint(i interface{}) string {
	s, _ := json.MarshalIndent(i, "", "\t")
	return string(s)
}

func main() {
	flag.Parse()

	loadConfig := new(packages.Config)
	loadConfig.Mode = packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo
	loadConfig.Fset = token.NewFileSet()
	pkgs, err := packages.Load(loadConfig, *inPkg)
	if err != nil {
		die(err.Error())
	}

	pkg, ts := findStruct(pkgs, *targetStruct)
	if ts == nil {
		die("Struct %s not found\n", *targetStruct)
	}

	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		die("Type %s is not a struct\n", ts.Name.Name)
	}

	sAttr := &structAttr{
		Name:   ts.Name.Name,
		Fields: []*fieldAttr{},
	}
	fillStructAttr(pkgs, pkg, st, sAttr)
	fmt.Println(prettyPrint(sAttr))
}

func fillStructAttr(pkgs []*packages.Package, pkg *packages.Package, st *ast.StructType, sAttr *structAttr) {
	for _, f := range st.Fields.List {
		tAttr := &typeAttr{}
		fillTypeAttr(pkgs, pkg, f.Type, tAttr)

		if len(f.Names) == 0 {
			sAttr.Fields = append(sAttr.Fields, &fieldAttr{
				FieldName: tAttr.Name,
				ValType:   tAttr,
			})
		} else {
			for _, n := range f.Names {
				sAttr.Fields = append(sAttr.Fields, &fieldAttr{
					FieldName: n.Name,
					ValType:   tAttr,
				})
			}
		}
	}
}

func fillTypeAttr(pkgs []*packages.Package, pkg *packages.Package, tExpr ast.Expr, tAttr *typeAttr) {
	switch t := tExpr.(type) {
	case *ast.Ident:
		tAttr.Name = t.Name
	case *ast.StarExpr:
		tAttr.IsPtr = true
		fillTypeAttr(pkgs, pkg, t.X, tAttr)
	case *ast.ArrayType:
		tAttr.IsSlice = true
		tAttr.SliceValType = &typeAttr{}
		fillTypeAttr(pkgs, pkg, t.Elt, tAttr.SliceValType)
	case *ast.MapType:
		tAttr.IsMap = true
		tAttr.MapKeyType = &typeAttr{}
		tAttr.MapValType = &typeAttr{}
		fillTypeAttr(pkgs, pkg, t.Key, tAttr.MapKeyType)
		fillTypeAttr(pkgs, pkg, t.Value, tAttr.MapValType)
	}

	if nt, ok := pkg.TypesInfo.Types[tExpr].Type.(*types.Named); ok {
		if _, ok := nt.Underlying().(*types.Struct); ok {
			tAttr.IsStruct = true
			tAttr.StructAttr = &structAttr{
				Name:   nt.Obj().Name(),
				Fields: []*fieldAttr{},
			}
			p, ts := findStruct(pkgs, nt.Obj().Name())
			if ts != nil {
				st := ts.Type.(*ast.StructType)
				fillStructAttr(pkgs, p, st, tAttr.StructAttr)
			}
		}
	}
}

func findStruct(pkgs []*packages.Package, structName string) (*packages.Package, *ast.TypeSpec) {
	for _, pkg := range pkgs {
		for _, syn := range pkg.Syntax {
			for _, f := range syn.Decls {
				gd, ok := f.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}

				for _, s := range gd.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if ok && ts.Name.Name == structName {
						return pkg, ts
					}
				}
			}
		}
	}

	return nil, nil
}
