package parser

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
)

// ParseFiles parses files at the same time
func ParseFiles(paths []string, withComments bool) ([]*GoFile, error) {
	files := make([]*ast.File, len(paths))
	fsets := make([]*token.FileSet, len(paths))
	for i, p := range paths {
		// File: A File node represents a Go source file: https://golang.org/pkg/go/ast/#File
		fset := token.NewFileSet()

		var mode parser.Mode
		if withComments {
			mode = parser.ParseComments
		} else {
			mode = 0
		}
		file, err := parser.ParseFile(fset, p, nil, mode)
		if err != nil {
			return nil, err
		}
		files[i] = file
		fsets[i] = fset
	}

	goFiles := make([]*GoFile, len(paths))
	for i, p := range paths {
		goFile, err := parseFile(p, nil, files[i], fsets[i], files)
		if err != nil {
			return nil, err
		}
		goFiles[i] = goFile
	}
	return goFiles, nil
}

// ParseSingleFile parses a single file at the same time
func ParseSingleFile(path string, withComments bool) (*GoFile, error) {
	fset := token.NewFileSet()

	var mode parser.Mode
	if withComments {
		mode = parser.ParseComments
	} else {
		mode = 0
	}
	file, err := parser.ParseFile(fset, path, nil, mode)
	if err != nil {
		return nil, err
	}
	return parseFile(path, nil, file, fset, []*ast.File{file})
}

func ParseSource(source string, filepath string, withComments bool) (*GoFile, error) {
	fset := token.NewFileSet()
	path := filepath
	var mode parser.Mode
	if withComments {
		mode = parser.ParseComments
	} else {
		mode = 0
	}
	file, err := parser.ParseFile(fset, path, source, mode)

	if err != nil {
		return nil, err
	}
	return parseFile(path, []byte(source), file, fset, []*ast.File{file})
}

func execCommand(name string, args ...string) (out string, exitCode int, err error) {
	stream := &strings.Builder{}

	cmd := exec.Command(name, args...)
	cmd.Stderr = stream
	cmd.Stdout = stream
	fmt.Printf("%v\n", strings.Join(cmd.Args, " "))

	err = cmd.Run()
	if err != nil {
		var terr *exec.ExitError
		if errors.As(err, &terr) {
			exitCode = terr.ExitCode()
			out = stream.String()
		}
	}

	fmt.Printf("Execution: %v\n", stream.String())

	return
}

func getLibrary(importUrl string) (err error, cleanup func()) {

	fmt.Printf("Importing %v\n", importUrl)

	cleanup = func() {}

	var out string
	var exitCode int

	_, staterr := os.Stat("go.mod")
	if os.IsNotExist(staterr) {
		out, exitCode, err = execCommand("go", "mod", "init", "tempmod")
		if err != nil {
			err = fmt.Errorf("failed to execute go mod init command to import Go library: %v.\nError: %v. Exit Code: %v\nOutput: %v\n", importUrl, err, exitCode, out)
			return
		}

		cleanup = func() {
			_ = os.Remove("go.mod")
			_ = os.Remove("go.sum")
		}
	}

	out, exitCode, err = execCommand("go", "get", "-v", importUrl)
	if err != nil {
		err = fmt.Errorf("failed to execute go get command to import Go library: %v.\nError: %v. Exit Code: %v\nOutput: %v\n", importUrl, err, exitCode, out)
		return
	}

	return
}

func parseFile(path string, source []byte, file *ast.File, fset *token.FileSet, files []*ast.File) (*GoFile, error) {

	var err error
	if source == nil {
		source, err = ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	// To import sources from vendor, we use "source" compile
	// https://github.com/golang/go/issues/11415#issuecomment-283445198
	conf := types.Config{Importer: &PackImporter{fset} /*importer.ForCompiler(fset, "source", nil)*/}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}

	conf.IgnoreFuncBodies = true

	tries := 2
	for tries > 0 {
		tries--
		if _, err = conf.Check(file.Name.Name, fset, files, info); err != nil {

			// Get package to import
			startingPointString := "could not import "

			start := strings.Index(err.Error(), startingPointString)
			if start > -1 && tries > 0 {
				start += len(startingPointString)
				end := strings.Index(err.Error()[start:], " ")

				if end > -1 {
					toimport := err.Error()[start : start+end]
					err, cleanup := getLibrary(toimport)
					defer cleanup()
					if err != nil {
						return nil, err
					}

					continue
				}
			}

			return nil, fmt.Errorf("errors type checking source file. error: %v", err)
		}
	}

	goFile := &GoFile{
		Path:    path,
		Package: file.Name.Name,
		Structs: []*GoStruct{},
	}

	// File.Decls: A list of the declarations in the file: https://golang.org/pkg/go/ast/#Decl
	for _, decl := range file.Decls {
		switch declType := decl.(type) {

		// GenDecl: represents an import, constant, type or variable declaration: https://golang.org/pkg/go/ast/#GenDecl
		case *ast.GenDecl:
			genDecl := declType

			// Specs: the Spec type stands for any of *ImportSpec, *ValueSpec, and *TypeSpec: https://golang.org/pkg/go/ast/#Spec
			for _, genSpec := range genDecl.Specs {
				switch genSpecType := genSpec.(type) {

				// A ValueSpec node represents a constant or variable declaration: https://pkg.go.dev/go/ast#ValueSpec
				case *ast.ValueSpec:
					valueSpec := genSpecType

					switch genDecl.Tok {
					case token.CONST:
						goConst := buildGoConstant(source, goFile, info, valueSpec)
						goFile.GlobalConstants = append(goFile.GlobalConstants, goConst)

					case token.VAR:
						goVar := buildGoVariable(source, goFile, info, valueSpec)
						goFile.GlobalVariables = append(goFile.GlobalVariables, goVar)
					}

				// TypeSpec: A TypeSpec node represents a type declaration: https://golang.org/pkg/go/ast/#TypeSpec
				case *ast.TypeSpec:
					typeSpec := genSpecType

					// typeSpec.Type: an Expr (expression) node: https://golang.org/pkg/go/ast/#Expr
					switch typeSpecType := typeSpec.Type.(type) {

					// StructType: A StructType node represents a struct type: https://golang.org/pkg/go/ast/#StructType
					case (*ast.StructType):
						structType := typeSpecType
						goStruct := buildGoStruct(source, goFile, info, typeSpec, structType, genDecl.Doc)
						goFile.Structs = append(goFile.Structs, goStruct)
					// InterfaceType: An InterfaceType node represents an interface type. https://golang.org/pkg/go/ast/#InterfaceType
					case (*ast.InterfaceType):
						interfaceType := typeSpecType
						goInterface := buildGoInterface(source, goFile, info, typeSpec, interfaceType, genDecl.Doc)
						goFile.Interfaces = append(goFile.Interfaces, goInterface)
					default:
						// a not-implemented typeSpec.Type.(type), ignore
					}
					// ImportSpec: An ImportSpec node represents a single package import. https://golang.org/pkg/go/ast/#ImportSpec
				case *ast.ImportSpec:
					importSpec := genSpec.(*ast.ImportSpec)
					goImport := buildGoImport(importSpec, goFile)
					goFile.Imports = append(goFile.Imports, goImport)
				default:
					// a not-implemented genSpec.(type), ignore
				}
			}
		case *ast.FuncDecl:
			funcDecl := declType
			goStructMethod := buildStructMethod(info, funcDecl, source, declType.Doc)
			goFile.StructMethods = append(goFile.StructMethods, goStructMethod)

		default:
			// a not-implemented decl.(type), ignore
		}
	}

	return goFile, nil
}

func buildGoVariable(source []byte, _ *GoFile, info *types.Info, spec *ast.ValueSpec) *GoType {
	var t *GoType
	if spec.Type == nil { // untyped const
		t = buildType(info, spec.Values[0], source)
	} else {
		t = buildType(info, spec.Type, source)
	}

	t.Name = spec.Names[0].Name

	return t
}

func buildGoConstant(source []byte, _ *GoFile, info *types.Info, spec *ast.ValueSpec) *GoType {

	var t *GoType
	if spec.Type == nil { // untyped const
		t = buildType(info, spec.Values[0], source)
	} else {
		t = buildType(info, spec.Type, source)
	}

	t.Name = spec.Names[0].Name

	return t
}

func buildGoImport(spec *ast.ImportSpec, file *GoFile) *GoImport {
	name := ""
	if spec.Name != nil {
		name = spec.Name.Name
	}

	path := ""
	if spec.Path != nil {
		path = spec.Path.Value
	}

	return &GoImport{
		Name: name,
		Path: path,
		File: file,
	}
}

func buildGoInterface(source []byte, file *GoFile, info *types.Info, typeSpec *ast.TypeSpec, interfaceType *ast.InterfaceType, cg *ast.CommentGroup) *GoInterface {
	goInterface := &GoInterface{
		File:     file,
		Name:     typeSpec.Name.Name,
		Methods:  buildMethodList(info, interfaceType.Methods.List, source),
		Comments: extractComment(cg),
	}

	return goInterface
}

func buildMethodList(info *types.Info, fieldList []*ast.Field, source []byte) []*GoMethod {
	methods := []*GoMethod{}

	for _, field := range fieldList {
		name := getNames(field)[0]

		fType, ok := field.Type.(*ast.FuncType)
		if !ok {
			// method was not a function
			continue
		}

		goMethod := &GoMethod{
			Name:    name,
			Params:  buildTypeList(info, fType.Params, source),
			Results: buildTypeList(info, fType.Results, source),
		}

		methods = append(methods, goMethod)
	}

	return methods
}

func buildStructMethod(info *types.Info, funcDecl *ast.FuncDecl, source []byte, cg *ast.CommentGroup) *GoStructMethod {
	return &GoStructMethod{
		Receivers: buildReceiverList(info, funcDecl.Recv, source),
		GoMethod: GoMethod{
			Name:     funcDecl.Name.Name,
			Params:   buildTypeList(info, funcDecl.Type.Params, source),
			Results:  buildTypeList(info, funcDecl.Type.Results, source),
			Comments: extractComment(cg),
		},
	}
}

func buildReceiverList(info *types.Info, fieldList *ast.FieldList, source []byte) []string {
	receivers := []string{}

	if fieldList != nil {
		for _, t := range fieldList.List {
			receivers = append(receivers, getTypeString(info, t.Type, source))
		}
	}

	return receivers
}

func buildTypeList(info *types.Info, fieldList *ast.FieldList, source []byte) []*GoType {
	types := []*GoType{}

	if fieldList != nil {
		for _, t := range fieldList.List {
			goType := buildType(info, t.Type, source)

			for _, n := range getNames(t) {
				copyType := copyType(goType)
				copyType.Name = n
				types = append(types, copyType)
			}
		}
	}

	return types
}

func getNames(field *ast.Field) []string {
	if field.Names == nil || len(field.Names) == 0 {
		return []string{""}
	}

	result := []string{}
	for _, name := range field.Names {
		result = append(result, name.String())
	}

	return result
}

func getTypeString(info *types.Info, expr ast.Expr, source []byte) string {

	if expr == nil {
		return ""
	}

	switch expr.(type) {
	case *ast.BasicLit:
		if typeInfo := info.TypeOf(expr); typeInfo != nil {
			return typeInfo.String()
		}

	case *ast.BinaryExpr:
		if typeInfo := info.TypeOf(expr); typeInfo != nil {
			return typeInfo.String()
		}

	default:
		return string(source[expr.Pos()-1 : expr.End()-1])
	}

	return ""
}

func getUnderlyingTypeString(info *types.Info, expr ast.Expr) string {
	if typeInfo := info.TypeOf(expr); typeInfo != nil {
		if underlying := typeInfo.Underlying(); underlying != nil {
			return underlying.String()
		}
	}

	return ""
}

func copyType(goType *GoType) *GoType {
	return &GoType{
		Type:       goType.Type,
		Inner:      goType.Inner,
		Name:       goType.Name,
		Underlying: goType.Underlying,
	}
}

func buildType(info *types.Info, expr ast.Expr, source []byte) *GoType {
	innerTypes := []*GoType{}
	typeString := getTypeString(info, expr, source)
	underlyingString := getUnderlyingTypeString(info, expr)

	switch specType := expr.(type) {
	case *ast.FuncType:
		innerTypes = append(innerTypes, buildTypeList(info, specType.Params, source)...)
		innerTypes = append(innerTypes, buildTypeList(info, specType.Results, source)...)
	case *ast.ArrayType:
		innerTypes = append(innerTypes, buildType(info, specType.Elt, source))
	case *ast.StructType:
		innerTypes = append(innerTypes, buildTypeList(info, specType.Fields, source)...)
	case *ast.MapType:
		innerTypes = append(innerTypes, buildType(info, specType.Key, source))
		innerTypes = append(innerTypes, buildType(info, specType.Value, source))
	case *ast.ChanType:
		innerTypes = append(innerTypes, buildType(info, specType.Value, source))
	case *ast.StarExpr:
		innerTypes = append(innerTypes, buildType(info, specType.X, source))
	case *ast.Ellipsis:
		innerTypes = append(innerTypes, buildType(info, specType.Elt, source))
	case *ast.InterfaceType:
		methods := buildMethodList(info, specType.Methods.List, source)
		for _, m := range methods {
			innerTypes = append(innerTypes, m.Params...)
			innerTypes = append(innerTypes, m.Results...)
		}

	case *ast.Ident:
	case *ast.SelectorExpr:
	case *ast.BasicLit:
	case *ast.BinaryExpr:
	default:
		fmt.Printf("Unexpected field type: `%s`,\n %#v\n", typeString, specType)
	}

	return &GoType{
		Type:       typeString,
		Underlying: underlyingString,
		Inner:      innerTypes,
	}
}

func buildGoStruct(source []byte, file *GoFile, info *types.Info, typeSpec *ast.TypeSpec, structType *ast.StructType, cg *ast.CommentGroup) *GoStruct {
	goStruct := &GoStruct{
		File:     file,
		Name:     typeSpec.Name.Name,
		Fields:   []*GoField{},
		Comments: extractComment(cg),
	}

	// Field: A Field declaration list in a struct type, a method list in an interface type,
	// or a parameter/result declaration in a signature: https://golang.org/pkg/go/ast/#Field
	for _, field := range structType.Fields.List {
		for _, name := range field.Names {
			goField := &GoField{
				Struct: goStruct,
				Name:   name.String(),
				Type:   string(source[field.Type.Pos()-1 : field.Type.End()-1]),
			}

			if field.Tag != nil {
				goTag := &GoTag{
					Field: goField,
					Value: field.Tag.Value,
				}

				goField.Tag = goTag
			}

			goStruct.Fields = append(goStruct.Fields, goField)
		}
	}

	return goStruct
}

func extractComment(cg *ast.CommentGroup) string {
	if cg == nil || cg.List == nil {
		return ""
	}

	var comment string
	for _, c := range cg.List {
		comment += c.Text
		comment = strings.ReplaceAll(comment, "//", "")
		comment = strings.ReplaceAll(comment, "/*", "")
		comment = strings.ReplaceAll(comment, "*/", "")
		comment = strings.TrimSpace(comment)
	}

	return comment
}
