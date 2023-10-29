package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"

	"golang.org/x/tools/go/packages"
)

var (
	inPkgs       = flag.String("inPkgs", "", "Go package to generate API for seperated by a comma")
	targetStruct = flag.String("structName", "", "Go struct to generate API for")
	outputFile   = flag.String("outFile", "", "Go file for generated API")
	outputPkg    = flag.String("outPkg", "", "Output package name for the generated files")
)

type (
	typeAttr struct {
		TypeName        string
		PkgPath         string
		IsPtr           bool
		IsSlice         bool
		IsMap           bool
		IsStruct        bool
		IsFunc          bool
		SliceValType    *typeAttr
		MapKeyType      *typeAttr
		MapValType      *typeAttr
		FuncParamTypes  []*typeAttr
		FuncResultTypes []*typeAttr
		StructFields    []*fieldAttr
	}

	fieldAttr struct {
		StructName string
		FieldName  string
		ValType    *typeAttr
	}

	structAttr struct {
		Pkg        *packages.Package
		TypeSpec   *ast.StructType
		StructName string
	}
)

func (t *typeAttr) String() string {
	s := ""
	if t.IsPtr {
		s += "*"
	}
	if t.IsSlice {
		s += "[]"
		s += t.SliceValType.String()
	} else if t.IsMap {
		s += "map[" + t.MapKeyType.String() + "]" + t.MapValType.String()
	} else if t.IsFunc {
		params := ""
		for i, ta := range t.FuncParamTypes {
			if i > 0 {
				params += ","
			}
			params += ta.String()
		}

		results := ""
		for i, ta := range t.FuncResultTypes {
			if i > 0 {
				results += ","
			}
			results += ta.String()
		}

		s += "func(" + params + ")" + "(" + results + ")"
	} else {
		if t.PkgPath != "" {
			if pkgPrefix, ok := pkgKeys[t.PkgPath]; ok {
				s += pkgPrefix + "."
			}
		}
		s += t.TypeName
	}
	return s
}

func (t *typeAttr) InitString() string {
	if !t.IsSlice && !t.IsMap {
		return ""
	}
	s := ""
	if t.IsPtr {
		s = "&" + t.String()[1:]
	} else {
		s = t.String()
	}
	return s + "{}"
}

const (
	preambleTmpltStr = `
package {{ .OutPkgName }}
{{- if gt (len .Imports) 0 }}
import (
	{{- range $k, $v := .Imports}}
	{{ $k }} "{{ $v }}"
	{{- end }}
)
{{- end }}
`

	builderTmpltStr = `
type {{ .TypeName }}Builder struct {
	s *{{ qualifiedName . }}
}

func New{{ .TypeName }}() *{{ .TypeName }}Builder {
	b := &{{ .TypeName }}Builder{
		s: &{{ qualifiedName . }}{},
	}
	{{- range .StructFields }} 
	{{- if or .ValType.IsSlice .ValType.IsMap}}
	b.s.{{ .FieldName }} = {{ typeInit .ValType }}
	{{- end }}
	{{- end }}
	return b
}

func (b *{{ .TypeName }}Builder) Build() *{{ qualifiedName . }} {
	return b.s
}
`

	withFuncTmpltStr = `
func (b *{{ .StructName }}Builder) With{{ .FieldName }}(a {{ .ValType }}) *{{ .StructName }}Builder {
	b.s.{{ .FieldName }} = a
	return b
}
	`

	addFuncTmpltStr = `
func (b *{{ .StructName }}Builder) Add{{ .FieldName }}(a {{ .ValType.SliceValType }}) *{{ .StructName }}Builder {
	b.s.{{ .FieldName }} = append(b.s.{{ .FieldName }}, a)
	return b
}
	`

	putFuncTmpltStr = `
func (b *{{ .StructName }}Builder) Put{{ .FieldName }}(k {{ .ValType.MapKeyType }}, v {{ .ValType.MapValType }}) *{{ .StructName }}Builder {
	b.s.{{ .FieldName }}[k] = v
	return b
}
	`
)

var (
	preambleTmplt *template.Template
	builderTmplt  *template.Template
	withFuncTmplt *template.Template
	addFuncTmplt  *template.Template
	putFuncTmplt  *template.Template

	pkgs          []*packages.Package
	pkgKeys       map[string]string
	pkgLoadConfig *packages.Config
	loadedPkgs    map[string]bool
	targetPkgs    []string
)

func init() {
	var err error

	preambleTmplt, err = template.New("preambleTmplt").Parse(preambleTmpltStr)
	if err != nil {
		die(err.Error())
	}

	builderTmplt, err = template.New("builderTmplt").Funcs(template.FuncMap{
		"typeInit": func(t *typeAttr) string {
			return t.InitString()
		},
		"qualifiedName": func(t *typeAttr) string {
			if k, ok := pkgKeys[t.PkgPath]; ok {
				return k + "." + t.TypeName
			}
			return t.TypeName
		},
	}).Parse(builderTmpltStr)
	if err != nil {
		die(err.Error())
	}

	withFuncTmplt, err = template.New("withFuncTmplt").Parse(withFuncTmpltStr)
	if err != nil {
		die(err.Error())
	}

	addFuncTmplt, err = template.New("addFuncTmplt").Parse(addFuncTmpltStr)
	if err != nil {
		die(err.Error())
	}

	putFuncTmplt, err = template.New("putFuncTmplt").Parse(putFuncTmpltStr)
	if err != nil {
		die(err.Error())
	}

	pkgLoadConfig = new(packages.Config)
	pkgLoadConfig.Mode = packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo
	pkgLoadConfig.Fset = token.NewFileSet()
	loadedPkgs = map[string]bool{}
	pkgs = []*packages.Package{}
}

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
	targetPkgs = strings.Split(*inPkgs, ",")
	loadPkgs(targetPkgs...)

	exportedStructs := findExportedStructs()
	toGenerate := []*typeAttr{}

	for _, es := range exportedStructs {
		sAttr := &typeAttr{
			TypeName:     es.StructName,
			PkgPath:      es.Pkg.ID,
			IsStruct:     true,
			StructFields: []*fieldAttr{},
		}

		toGenerate = append(toGenerate, sAttr)
		fillStructAttr(es.Pkg, es.TypeSpec, sAttr)
	}

	imports := collectImports()
	pkgKeys = make(map[string]string)
	for k, v := range imports {
		pkgKeys[v] = k
	}

	outDir := filepath.Dir(*outputFile)
	os.MkdirAll(outDir, os.ModePerm)

	var buff bytes.Buffer
	if err := preambleTmplt.Execute(&buff, struct {
		OutPkgName string
		Imports    *map[string]string
	}{
		OutPkgName: *outputPkg,
		Imports:    &imports,
	}); err != nil {
		die(err.Error())
	}

	for _, s := range toGenerate {
		if err := builderTmplt.Execute(&buff, s); err != nil {
			die(err.Error())
		}

		for _, sf := range s.StructFields {
			if err := withFuncTmplt.Execute(&buff, sf); err != nil {
				die(err.Error())
			}

			if sf.ValType.IsSlice {
				if err := addFuncTmplt.Execute(&buff, sf); err != nil {
					die(err.Error())
				}
			} else if sf.ValType.IsMap {
				if err := putFuncTmplt.Execute(&buff, sf); err != nil {
					die(err.Error())
				}
			}
		}
	}

	if err := os.WriteFile(*outputFile, buff.Bytes(), 0644); err != nil {
		die(err.Error())
	}
}

func loadPkgs(path ...string) {
	ps, err := packages.Load(pkgLoadConfig, path...)
	if err != nil {
		die(err.Error())
	}

	for _, p := range ps {
		if _, ok := loadedPkgs[p.ID]; !ok {
			pkgs = append(pkgs, p)
			loadedPkgs[p.ID] = true
		}
	}

}

func fillStructAttr(pkg *packages.Package, st *ast.StructType, sAttr *typeAttr) {
	for _, f := range st.Fields.List {
		tAttr := &typeAttr{}
		fillTypeAttr(pkg, f.Type, tAttr)

		if len(f.Names) == 0 && isNameExported(sAttr.TypeName) {
			sAttr.StructFields = append(sAttr.StructFields, &fieldAttr{
				StructName: sAttr.TypeName,
				FieldName:  tAttr.TypeName,
				ValType:    tAttr,
			})
		} else {
			for _, n := range f.Names {
				if isNameExported(n.Name) {
					sAttr.StructFields = append(sAttr.StructFields, &fieldAttr{
						StructName: sAttr.TypeName,
						FieldName:  n.Name,
						ValType:    tAttr,
					})
				}
			}
		}
	}
}

func fillTypeAttr(pkg *packages.Package, tExpr ast.Expr, tAttr *typeAttr) {
	switch t := tExpr.(type) {
	case *ast.Ident:
		tAttr.TypeName = t.Name
	case *ast.StarExpr:
		tAttr.IsPtr = true
		fillTypeAttr(pkg, t.X, tAttr)
	case *ast.ArrayType:
		tAttr.IsSlice = true
		tAttr.SliceValType = &typeAttr{}
		fillTypeAttr(pkg, t.Elt, tAttr.SliceValType)
	case *ast.MapType:
		tAttr.IsMap = true
		tAttr.MapKeyType = &typeAttr{}
		tAttr.MapValType = &typeAttr{}
		fillTypeAttr(pkg, t.Key, tAttr.MapKeyType)
		fillTypeAttr(pkg, t.Value, tAttr.MapValType)
	case *ast.FuncType:
		tAttr.IsFunc = true
		tAttr.FuncParamTypes = []*typeAttr{}
		tAttr.FuncResultTypes = []*typeAttr{}

		for _, p := range t.Params.List {
			pAttr := &typeAttr{}
			fillTypeAttr(pkg, p.Type, pAttr)
			tAttr.FuncParamTypes = append(tAttr.FuncParamTypes, pAttr)
		}

		if t.Results != nil {
			for _, p := range t.Results.List {
				pAttr := &typeAttr{}
				fillTypeAttr(pkg, p.Type, pAttr)
				tAttr.FuncResultTypes = append(tAttr.FuncResultTypes, pAttr)
			}
		}
	}

	if nt, ok := pkg.TypesInfo.Types[tExpr].Type.(*types.Named); ok {
		tAttr.TypeName = nt.Obj().Name()
		if nt.Obj().Pkg() != nil {
			tAttr.PkgPath = nt.Obj().Pkg().Path()
			if _, ok := loadedPkgs[tAttr.PkgPath]; !ok {
				loadPkgs(tAttr.PkgPath)
			}
		}
	}
}

func findExportedStructs() []*structAttr {
	structs := []*structAttr{}
	for _, pkg := range pkgs {
		for _, syn := range pkg.Syntax {
			for _, f := range syn.Decls {
				gd, ok := f.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}

				for _, s := range gd.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if ok && isNameExported(ts.Name.Name) {
						if st, ok := ts.Type.(*ast.StructType); ok {
							structs = append(structs, &structAttr{
								StructName: ts.Name.Name,
								Pkg:        pkg,
								TypeSpec:   st,
							})
						}
					}
				}
			}
		}
	}
	return structs
}

func collectImports() map[string]string {
	ret := map[string]string{}
	m := map[string]map[string]bool{}
	for v := range loadedPkgs {
		pkgParts := strings.Split(v, "/")
		pkgKey := pkgParts[len(pkgParts)-1]
		if _, ok := m[pkgKey]; !ok {
			m[pkgKey] = map[string]bool{}
		}
		m[pkgKey][v] = true
	}

	for pkgKey, pkgs := range m {
		i := 0
		for pkgPath := range pkgs {
			if i > 0 {
				ret[fmt.Sprintf("%s_%d", pkgKey, i)] = pkgPath
			} else {
				ret[pkgKey] = pkgPath
			}
			i++
		}
	}
	return ret
}

func isNameExported(n string) bool {
	return len(n) > 0 && unicode.IsUpper(rune(n[0]))
}
