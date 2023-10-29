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
	inPkgs     = flag.String("pkgs", "", "Go packages containing the structs to generate the API for")
	outputFile = flag.String("out", "", "Output go file path for the generated API")
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
	neededPkgs    map[string]bool
	targetPkgs    []string
)

func init() {
	var err error

	preambleTmplt, err = template.New("preambleTmplt").Parse(preambleTmpltStr)
	if err != nil {
		die("%v\n", err)
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
		die("%v\n", err)
	}

	withFuncTmplt, err = template.New("withFuncTmplt").Parse(withFuncTmpltStr)
	if err != nil {
		die("%v\n", err)
	}

	addFuncTmplt, err = template.New("addFuncTmplt").Parse(addFuncTmpltStr)
	if err != nil {
		die("%v\n", err)
	}

	putFuncTmplt, err = template.New("putFuncTmplt").Parse(putFuncTmpltStr)
	if err != nil {
		die("%v\n", err)
	}

	pkgLoadConfig = new(packages.Config)
	pkgLoadConfig.Mode = packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo
	pkgLoadConfig.Fset = token.NewFileSet()
	loadedPkgs = map[string]bool{}
	neededPkgs = map[string]bool{}
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
	if *inPkgs == "" {
		die("missing required -pkgs arg\n")
	}
	if *outputFile == "" {
		die("missing required -out arg\n")
	}

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
		neededPkgs[es.Pkg.ID] = true
		toGenerate = append(toGenerate, sAttr)
		fillStructAttr(es.Pkg, es.TypeSpec, sAttr)
	}

	imports := collectImports()
	pkgKeys = make(map[string]string)
	for k, v := range imports {
		pkgKeys[v] = k
	}

	outDir := filepath.Dir(*outputFile)
	outPkg := filepath.Base(outDir)
	os.MkdirAll(outDir, os.ModePerm)

	var buff bytes.Buffer
	if err := preambleTmplt.Execute(&buff, struct {
		OutPkgName string
		Imports    *map[string]string
	}{
		OutPkgName: outPkg,
		Imports:    &imports,
	}); err != nil {
		die("%v\n", err)
	}

	for _, s := range toGenerate {
		if err := builderTmplt.Execute(&buff, s); err != nil {
			die("%v\n", err)
		}

		for _, sf := range s.StructFields {
			if err := withFuncTmplt.Execute(&buff, sf); err != nil {
				die("%v\n", err)
			}

			if sf.ValType.IsSlice {
				if err := addFuncTmplt.Execute(&buff, sf); err != nil {
					die("%v\n", err)
				}
			} else if sf.ValType.IsMap {
				if err := putFuncTmplt.Execute(&buff, sf); err != nil {
					die("%v\n", err)
				}
			}
		}
	}

	if err := os.WriteFile(*outputFile, buff.Bytes(), 0644); err != nil {
		die("%v\n", err)
	}
}

func loadPkgs(path ...string) {
	ps, err := packages.Load(pkgLoadConfig, path...)
	if err != nil {
		die("%v\n", err)
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
		if !fillTypeAttr(pkg, f.Type, tAttr) {
			continue
		}

		if len(f.Names) == 0 {
			sAttr.StructFields = append(sAttr.StructFields, &fieldAttr{
				StructName: sAttr.TypeName,
				FieldName:  tAttr.TypeName,
				ValType:    tAttr,
			})

			if tAttr.PkgPath != "" {
				neededPkgs[tAttr.PkgPath] = true
			}
		} else {
			for _, n := range f.Names {
				if isNameExported(n.Name) {
					sAttr.StructFields = append(sAttr.StructFields, &fieldAttr{
						StructName: sAttr.TypeName,
						FieldName:  n.Name,
						ValType:    tAttr,
					})

					if tAttr.PkgPath != "" {
						neededPkgs[tAttr.PkgPath] = true
					}
				}
			}
		}
	}
}

func fillTypeAttr(pkg *packages.Package, tExpr ast.Expr, tAttr *typeAttr) bool {
	switch t := tExpr.(type) {
	case *ast.Ident:
		tAttr.TypeName = t.Name
	case *ast.StarExpr:
		tAttr.IsPtr = true
		if !fillTypeAttr(pkg, t.X, tAttr) {
			return false
		}
	case *ast.ArrayType:
		tAttr.IsSlice = true
		tAttr.SliceValType = &typeAttr{}
		if !fillTypeAttr(pkg, t.Elt, tAttr.SliceValType) {
			return false
		}
	case *ast.MapType:
		tAttr.IsMap = true
		tAttr.MapKeyType = &typeAttr{}
		tAttr.MapValType = &typeAttr{}
		if !fillTypeAttr(pkg, t.Key, tAttr.MapKeyType) {
			return false
		}
		if !fillTypeAttr(pkg, t.Value, tAttr.MapValType) {
			return false
		}
	case *ast.FuncType:
		tAttr.IsFunc = true
		tAttr.FuncParamTypes = []*typeAttr{}
		tAttr.FuncResultTypes = []*typeAttr{}

		for _, p := range t.Params.List {
			pAttr := &typeAttr{}
			if !fillTypeAttr(pkg, p.Type, pAttr) {
				return false
			}
			tAttr.FuncParamTypes = append(tAttr.FuncParamTypes, pAttr)
		}

		if t.Results != nil {
			for _, p := range t.Results.List {
				pAttr := &typeAttr{}
				if !fillTypeAttr(pkg, p.Type, pAttr) {
					return false
				}
				tAttr.FuncResultTypes = append(tAttr.FuncResultTypes, pAttr)
			}
		}
	}

	if nt, ok := pkg.TypesInfo.Types[tExpr].Type.(*types.Named); ok {
		tAttr.TypeName = nt.Obj().Name()
		if !isNameExported(tAttr.TypeName) {
			return false
		}

		if nt.Obj().Pkg() != nil {
			tAttr.PkgPath = nt.Obj().Pkg().Path()
			if isInternalPkg(tAttr.PkgPath) {
				return false
			}

			if _, ok := loadedPkgs[tAttr.PkgPath]; !ok {
				loadPkgs(tAttr.PkgPath)
			}
		}
	}

	return true
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
	for v := range neededPkgs {
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

func isInternalPkg(p string) bool {
	return strings.Contains(p, "/internal/")
}
