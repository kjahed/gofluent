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

	"golang.org/x/tools/go/packages"
)

var (
	inPkg        = flag.String("inPkg", "", "Go package to generate API for")
	targetStruct = flag.String("structName", "", "Go struct to generate API for")
	outputDir    = flag.String("outDir", "", "Output directory for the generated files")
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

	pkgKeys map[string]string
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

	sAttr := &typeAttr{
		TypeName:     ts.Name.Name,
		PkgPath:      pkg.ID,
		IsStruct:     true,
		StructFields: []*fieldAttr{},
	}

	fillStructAttr(pkgs, pkg, st, sAttr)

	fmt.Println(prettyPrint(sAttr))

	imports := collectPackages(sAttr)
	pkgKeys = make(map[string]string)
	for k, v := range imports {
		pkgKeys[v] = k
	}

	os.MkdirAll(*outputDir, os.ModePerm)

	var buff bytes.Buffer
	err = preambleTmplt.Execute(&buff, struct {
		OutPkgName string
		Imports    *map[string]string
	}{
		OutPkgName: *outputPkg,
		Imports:    &imports,
	})
	if err != nil {
		die(err.Error())
	}

	err = builderTmplt.Execute(&buff, sAttr)
	if err != nil {
		die(err.Error())
	}

	for _, sf := range sAttr.StructFields {
		err = withFuncTmplt.Execute(&buff, sf)
		if err != nil {
			die(err.Error())
		}

		if sf.ValType.IsSlice {
			err = addFuncTmplt.Execute(&buff, sf)
			if err != nil {
				die(err.Error())
			}
		} else if sf.ValType.IsMap {
			err = putFuncTmplt.Execute(&buff, sf)
			if err != nil {
				die(err.Error())
			}
		}
	}

	os.WriteFile(filepath.Join(*outputDir, "fluent.go"), buff.Bytes(), 0644)

	fmt.Println(buff.String())

}

func fillStructAttr(pkgs []*packages.Package, pkg *packages.Package, st *ast.StructType, sAttr *typeAttr) {
	for _, f := range st.Fields.List {
		tAttr := &typeAttr{}
		fillTypeAttr(pkgs, pkg, f.Type, tAttr)

		if len(f.Names) == 0 {
			sAttr.StructFields = append(sAttr.StructFields, &fieldAttr{
				StructName: sAttr.TypeName,
				FieldName:  tAttr.TypeName,
				ValType:    tAttr,
			})
		} else {
			for _, n := range f.Names {
				sAttr.StructFields = append(sAttr.StructFields, &fieldAttr{
					StructName: sAttr.TypeName,
					FieldName:  n.Name,
					ValType:    tAttr,
				})
			}
		}
	}
}

func fillTypeAttr(pkgs []*packages.Package, pkg *packages.Package, tExpr ast.Expr, tAttr *typeAttr) {
	switch t := tExpr.(type) {
	case *ast.Ident:
		tAttr.TypeName = t.Name
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
	case *ast.FuncType:
		tAttr.IsFunc = true
		tAttr.FuncParamTypes = []*typeAttr{}
		tAttr.FuncResultTypes = []*typeAttr{}

		for _, p := range t.Params.List {
			pAttr := &typeAttr{}
			fillTypeAttr(pkgs, pkg, p.Type, pAttr)
			tAttr.FuncParamTypes = append(tAttr.FuncParamTypes, pAttr)
		}

		if t.Results != nil {
			for _, p := range t.Results.List {
				pAttr := &typeAttr{}
				fillTypeAttr(pkgs, pkg, p.Type, pAttr)
				tAttr.FuncResultTypes = append(tAttr.FuncResultTypes, pAttr)
			}
		}
	}

	if nt, ok := pkg.TypesInfo.Types[tExpr].Type.(*types.Named); ok {
		if _, ok := nt.Underlying().(*types.Struct); ok {
			tAttr.IsStruct = true
			p, ts := findStruct(pkgs, nt.Obj().Name())
			if ts != nil {
				tAttr.TypeName = nt.Obj().Name()
				tAttr.PkgPath = p.ID
				tAttr.StructFields = []*fieldAttr{}
				st := ts.Type.(*ast.StructType)
				fillStructAttr(pkgs, p, st, tAttr)
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

func collectPackages(t *typeAttr) map[string]string {
	ret := map[string]string{}
	m := map[string]map[string]bool{}
	collectPkgsHelper(t, &m)
	for pkgKey, pkgs := range m {
		i := 0
		for pkgPath := range pkgs {
			if i > 0 {
				ret[fmt.Sprintf("%s_%d", pkgKey, i)] = pkgPath
			} else {
				ret[pkgKey] = pkgPath
			}
		}
	}
	return ret
}

func collectPkgsHelper(t *typeAttr, m *map[string]map[string]bool) {
	mp := *m
	pkgParts := strings.Split(t.PkgPath, "/")
	pkgKey := pkgParts[len(pkgParts)-1]

	if _, ok := mp[pkgKey]; !ok {
		mp[pkgKey] = map[string]bool{}
	}
	mp[pkgKey][t.PkgPath] = true

	for _, fa := range t.StructFields {
		if fa.ValType.IsStruct {
			collectPkgsHelper(fa.ValType, m)
		}
	}
}
