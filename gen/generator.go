package gen

import (
	"bytes"
	"encoding/json"
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

type (
	GeneratorConfig struct {
		Pkgs   []string
		OutDir string
	}
)

type (
	typeAttr struct {
		TypeName        string
		PkgPath         string
		PkgName         string
		DefiningFile    string
		IsPtr           bool
		IsSlice         bool
		IsMap           bool
		IsStruct        bool
		IsFunc          bool
		IsIntf          bool
		HasBuilder      bool
		SliceValType    *typeAttr
		MapKeyType      *typeAttr
		MapValType      *typeAttr
		FuncParamTypes  []*typeAttr
		FuncResultTypes []*typeAttr
		StructFields    []*fieldAttr
		Implementations []*typeAttr
	}

	fieldAttr struct {
		StructName string
		FieldName  string
		FuncSuffix string
		ValType    *typeAttr
	}

	structAttr struct {
		Pkg        *packages.Package
		TypeSpec   *ast.StructType
		StructName string
	}
)

func (f *fieldAttr) getVariations() []*fieldAttr {
	v := []*fieldAttr{}
	if f.ValType.IsIntf {
		for _, ta := range f.ValType.Implementations {
			v = append(v, &fieldAttr{
				StructName: f.StructName,
				FieldName:  f.FieldName,
				FuncSuffix: "_" + ta.TypeName,
				ValType:    ta,
			})
		}
	} else {
		v = append(v, f)
	}
	return v
}

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
	} else if t.HasBuilder {
		if !t.IsPtr {
			s = "*" + s
		}
		s += t.TypeName + "Builder"
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
// Code generated by gofluent. DO NOT EDIT.
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

func From{{ .TypeName }}(a *{{ qualifiedName . }}) *{{ .TypeName }}Builder {
	b := &{{ .TypeName }}Builder{
		s: a,
	}
	return b
}

func (b *{{ .TypeName }}Builder) Build() *{{ qualifiedName . }} {
	return b.s
}
`

	withFuncTmpltStr = `
func (b *{{ .StructName }}Builder) With{{ .FieldName }}{{ .FuncSuffix }}(a {{ .ValType }}) *{{ .StructName }}Builder {
	b.s.{{ .FieldName }} = a{{ if .ValType.HasBuilder }}.Build(){{ end }}
	return b
}
	`

	addFuncTmpltStr = `
func (b *{{ .StructName }}Builder) Add{{ .FieldName }}(a {{ .ValType.SliceValType }}) *{{ .StructName }}Builder {
	b.s.{{ .FieldName }} = append(b.s.{{ .FieldName }}, a{{ if .ValType.SliceValType.HasBuilder }}.Build(){{ end }})
	return b
}
	`

	putFuncTmpltStr = `
func (b *{{ .StructName }}Builder) Put{{ .FieldName }}(k {{ .ValType.MapKeyType }}, v {{ .ValType.MapValType }}) *{{ .StructName }}Builder {
	b.s.{{ .FieldName }}[k{{ if .ValType.MapKeyType.HasBuilder }}.Build(){{ end }}] = v{{ if .ValType.MapValType.HasBuilder }}.Build(){{ end }}
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
		panic(err)
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
		panic(err)
	}

	withFuncTmplt, err = template.New("withFuncTmplt").Parse(withFuncTmpltStr)
	if err != nil {
		panic(err)
	}

	addFuncTmplt, err = template.New("addFuncTmplt").Parse(addFuncTmpltStr)
	if err != nil {
		panic(err)
	}

	putFuncTmplt, err = template.New("putFuncTmplt").Parse(putFuncTmpltStr)
	if err != nil {
		panic(err)
	}

	pkgLoadConfig = new(packages.Config)
	pkgLoadConfig.Mode = packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesSizes | packages.NeedTypesInfo | packages.NeedName | packages.NeedDeps | packages.NeedImports
	pkgLoadConfig.Fset = token.NewFileSet()
	loadedPkgs = map[string]bool{}
	pkgs = []*packages.Package{}
}

func prettyPrint(i interface{}) string {
	s, _ := json.MarshalIndent(i, "", "\t")
	return string(s)
}

func Generate(conf *GeneratorConfig) error {
	if err := loadPkgs(conf.Pkgs...); err != nil {
		return err
	}

	exportedStructs := findExportedStructs(pkgs)
	toGenerate := map[string][]*typeAttr{}

	for _, es := range exportedStructs {
		sAttr := &typeAttr{
			TypeName:     es.StructName,
			PkgPath:      es.Pkg.ID,
			PkgName:      es.Pkg.Name,
			DefiningFile: es.Pkg.Fset.File(es.TypeSpec.Pos()).Name(),
			IsStruct:     true,
			StructFields: []*fieldAttr{},
		}
		if _, ok := toGenerate[es.Pkg.ID]; !ok {
			toGenerate[es.Pkg.ID] = []*typeAttr{}
		}
		toGenerate[es.Pkg.ID] = append(toGenerate[es.Pkg.ID], sAttr)
		if err := fillStructAttr(es.Pkg, es.TypeSpec, sAttr); err != nil {
			return err
		}
	}

	for _, ss := range toGenerate {
		for _, s := range ss {
			fillHasBuilder(s, toGenerate)
		}
	}

	for _, ss := range toGenerate {
		outDir, err := filepath.Abs(conf.OutDir)
		if err != nil {
			return err
		}

		outPkg := filepath.Base(outDir)
		outFile := ss[0].PkgName + "_fluent.go"

		imports := collectImports(ss)
		pkgKeys = make(map[string]string)
		for k, v := range imports {
			pkgKeys[v] = k
		}

		os.MkdirAll(outDir, os.ModePerm)

		var buff bytes.Buffer
		if err := preambleTmplt.Execute(&buff, struct {
			OutPkgName string
			Imports    *map[string]string
		}{
			OutPkgName: outPkg,
			Imports:    &imports,
		}); err != nil {
			return err
		}

		for _, s := range ss {
			if err := builderTmplt.Execute(&buff, s); err != nil {
				return err
			}

			for _, sf := range s.StructFields {
				for _, fa := range sf.getVariations() {
					if err := withFuncTmplt.Execute(&buff, fa); err != nil {
						return err
					}
				}

				if sf.ValType.IsSlice {
					if err := addFuncTmplt.Execute(&buff, sf); err != nil {
						return err
					}
				} else if sf.ValType.IsMap {
					if err := putFuncTmplt.Execute(&buff, sf); err != nil {
						return err
					}
				}
			}
		}

		if err := os.WriteFile(filepath.Join(outDir, outFile), buff.Bytes(), 0644); err != nil {
			return err
		}
	}

	return nil
}

func loadPkgs(path ...string) error {
	ps, err := packages.Load(pkgLoadConfig, path...)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if _, ok := loadedPkgs[p.ID]; !ok {
			pkgs = append(pkgs, p)
			loadedPkgs[p.ID] = true
		}
	}
	return nil
}

func fillStructAttr(pkg *packages.Package, st *ast.StructType, sAttr *typeAttr) error {
	for _, f := range st.Fields.List {
		tAttr := &typeAttr{}
		if ok, err := fillTypeAttr(pkg, f.Type, tAttr); !ok || err != nil {
			return err
		}

		if len(f.Names) == 0 {
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
	return nil
}

func fillTypeAttr(pkg *packages.Package, tExpr ast.Expr, tAttr *typeAttr) (bool, error) {
	switch t := tExpr.(type) {
	case *ast.Ident:
		tAttr.TypeName = t.Name
	case *ast.StarExpr:
		tAttr.IsPtr = true
		if ok, err := fillTypeAttr(pkg, t.X, tAttr); !ok || err != nil {
			return false, nil
		}
	case *ast.ArrayType:
		tAttr.IsSlice = true
		tAttr.SliceValType = &typeAttr{}
		if ok, err := fillTypeAttr(pkg, t.Elt, tAttr.SliceValType); !ok || err != nil {
			return false, err
		}
	case *ast.MapType:
		tAttr.IsMap = true
		tAttr.MapKeyType = &typeAttr{}
		tAttr.MapValType = &typeAttr{}
		if ok, err := fillTypeAttr(pkg, t.Key, tAttr.MapKeyType); !ok || err != nil {
			return false, err
		}
		if ok, err := fillTypeAttr(pkg, t.Value, tAttr.MapValType); !ok || err != nil {
			return false, err
		}
	case *ast.FuncType:
		tAttr.IsFunc = true
		tAttr.FuncParamTypes = []*typeAttr{}
		tAttr.FuncResultTypes = []*typeAttr{}

		for _, p := range t.Params.List {
			pAttr := &typeAttr{}
			if ok, err := fillTypeAttr(pkg, p.Type, pAttr); !ok || err != nil {
				return false, err
			}
			tAttr.FuncParamTypes = append(tAttr.FuncParamTypes, pAttr)
		}

		if t.Results != nil {
			for _, p := range t.Results.List {
				pAttr := &typeAttr{}
				if ok, err := fillTypeAttr(pkg, p.Type, pAttr); !ok || err != nil {
					return false, err
				}
				tAttr.FuncResultTypes = append(tAttr.FuncResultTypes, pAttr)
			}
		}
	}

	tp := pkg.TypesInfo.Types[tExpr].Type
	if nt, ok := tp.(*types.Named); ok {
		tAttr.TypeName = nt.Obj().Name()
		if nt.Obj().Pkg() != nil {
			tAttr.PkgPath = nt.Obj().Pkg().Path()
			tAttr.PkgName = nt.Obj().Pkg().Name()
			if isInternalPkg(tAttr.PkgPath) {
				return false, nil
			}
			if _, ok := loadedPkgs[tAttr.PkgPath]; !ok {
				if err := loadPkgs(tAttr.PkgPath); err != nil {
					return false, err
				}
			}
		}

		if !isNameExported(tAttr.TypeName) {
			if it, ok := tp.Underlying().(*types.Interface); ok {
				tAttr.IsIntf = true
				tAttr.Implementations = findImplementations([]*packages.Package{pkg}, it)
				return len(tAttr.Implementations) > 0, nil
			}
			return false, nil
		}
	}
	return true, nil
}

func findExportedStructs(pkgs []*packages.Package) []*structAttr {
	structs := []*structAttr{}
	for _, p := range pkgs {
		for _, ts := range findStructsInPkg(p) {
			if st, ok := ts.Type.(*ast.StructType); ok {
				if !isNameExported(ts.Name.Name) {
					continue
				}

				structs = append(structs, &structAttr{
					StructName: ts.Name.Name,
					Pkg:        p,
					TypeSpec:   st,
				})
			}
		}
	}
	return structs
}

// ita := &typeAttr{
// 	TypeName:     sa.StructName,
// 	PkgPath:      sa.Pkg.ID,
// 	PkgName:      sa.Pkg.Name,
// 	DefiningFile: sa.Pkg.Fset.File(sa.TypeSpec.Pos()).Name(),
// 	IsPtr:        true,
// 	IsStruct:     true,
// }
// if fillTypeAttr(sa.Pkg, sa.TypeSpec, ita) {
// 	tAttr.Implementations = append(tAttr.Implementations, ita)
// }

func findImplementations(pkgs []*packages.Package, intf *types.Interface) []*typeAttr {
	implementations := []*typeAttr{}
	exportedStructs := findExportedStructs(pkgs)
	for _, sa := range exportedStructs {
		st := sa.Pkg.Types.Scope().Lookup(sa.StructName)
		if st != nil {
			tattr := &typeAttr{
				TypeName:     sa.StructName,
				PkgPath:      sa.Pkg.ID,
				PkgName:      sa.Pkg.Name,
				DefiningFile: sa.Pkg.Fset.File(sa.TypeSpec.Pos()).Name(),
				IsStruct:     true,
			}
			fillTypeAttr(sa.Pkg, sa.TypeSpec, tattr)

			if types.Implements(st.Type(), intf) {
				implementations = append(implementations, tattr)
			} else if types.Implements(types.NewPointer(st.Type()), intf) {
				tattr.IsPtr = true
				implementations = append(implementations, tattr)
			}
		}
	}
	return implementations
}

func findStructsInPkg(pkg *packages.Package) []*ast.TypeSpec {
	typeSpecs := []*ast.TypeSpec{}
	for _, syn := range pkg.Syntax {
		for _, f := range syn.Decls {
			gd, ok := f.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok {
					if _, ok := ts.Type.(*ast.StructType); ok {
						typeSpecs = append(typeSpecs, ts)
					}
				}
			}
		}
	}
	return typeSpecs
}

func fillHasBuilder(t *typeAttr, m map[string][]*typeAttr) {
	if t == nil {
		return
	}

	if t.IsStruct {
		for _, ss := range m {
			for _, ta := range ss {
				if t.TypeName == ta.TypeName {
					t.HasBuilder = true
				}
			}
		}
	}

	fillHasBuilder(t.MapKeyType, m)
	fillHasBuilder(t.MapValType, m)
	fillHasBuilder(t.SliceValType, m)
	for _, ta := range t.FuncParamTypes {
		fillHasBuilder(ta, m)
	}
	for _, ta := range t.FuncResultTypes {
		fillHasBuilder(ta, m)
	}
	for _, fa := range t.StructFields {
		fillHasBuilder(fa.ValType, m)
	}
	for _, ta := range t.Implementations {
		fillHasBuilder(ta, m)
	}
}

func collectImports(t []*typeAttr) map[string]string {
	ret := map[string]string{}
	m := map[string]map[string]bool{}
	for v := range collectRequiredPkgs(t) {
		pkgKey := pkgNameFromPath(v)
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

func collectRequiredPkgs(t []*typeAttr) map[string]bool {
	m := map[string]bool{}
	for _, ta := range t {
		collectRequiredPkgsHelper(ta, &m)
	}
	return m
}

func collectRequiredPkgsHelper(t *typeAttr, m *map[string]bool) {
	if t == nil {
		return
	}
	if t.PkgPath != "" {
		(*m)[t.PkgPath] = true
	}
	collectRequiredPkgsHelper(t.MapKeyType, m)
	collectRequiredPkgsHelper(t.MapValType, m)
	collectRequiredPkgsHelper(t.SliceValType, m)
	for _, ta := range t.FuncParamTypes {
		collectRequiredPkgsHelper(ta, m)
	}
	for _, ta := range t.FuncResultTypes {
		collectRequiredPkgsHelper(ta, m)
	}
	for _, fa := range t.StructFields {
		collectRequiredPkgsHelper(fa.ValType, m)
	}
	for _, ta := range t.Implementations {
		collectRequiredPkgsHelper(ta, m)
	}
}

func pkgNameFromPath(p string) string {
	pkgParts := strings.Split(p, "/")
	return pkgParts[len(pkgParts)-1]
}

func isNameExported(n string) bool {
	return len(n) > 0 && unicode.IsUpper(rune(n[0]))
}

func isInternalPkg(p string) bool {
	return strings.Contains(p, "/internal/")
}