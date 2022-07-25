package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

const (
	_columnPrefix = "column:"
	_primaryKey1  = "primaryKey"
	_primaryKey2  = "primary_key"
	_goFileExt    = ".go"
	_string       = "string"
)

var (
	modelPath          string
	generateFilePrefix string
	once               sync.Once
	packageMap         = make(map[string]interface{})
)

var Build CodeBuilder

type CodeBuilder interface {
	BuildTemplateMap(*ModelInfo) map[string]interface{}
	PackageKey() string
	HeaderTemplate() string
	ContentTemplate() string
	FileMaxSize() int
}

type fileInfos []*ast.File

type ModelFieldInfo struct {
	Name       string
	Type       string
	Column     string
	PrimaryKey bool
	Comments   []string
}

type ModelInfo struct {
	Name     string
	Fields   []ModelFieldInfo
	Comments []string
}

type modelInfos []ModelInfo

func (files fileInfos) getModelObjects() (objects []*ast.Object) {
	for _, file := range files {
		once.Do(func() {
			packageMap[Build.PackageKey()] = file.Name.Name
		})
		for _, v := range file.Decls {
			if decl, ok := v.(*ast.FuncDecl); ok {
				if obj, ok := isTablerFunc(decl); ok {
					objects = append(objects, obj)
				}
			}
		}
	}
	return
}

func (models modelInfos) generateFile() error {
	maxSize := Build.FileMaxSize()
	hTemplate, err := template.New("").Parse(Build.HeaderTemplate())
	if err != nil {
		return err
	}
	cTemplate, err := template.New("").Parse(Build.ContentTemplate())
	if err != nil {
		return err
	}
	buffer := bytes.Buffer{}
	index := 0
	setHeader := false
	setFileHeader := func() error {
		if err := hTemplate.Execute(&buffer, packageMap); err != nil {
			return err
		}
		setHeader = true
		return nil
	}
	save := func() error {
		filePath := path.Join(modelPath, fmt.Sprintf("%s%d%s", generateFilePrefix, index, _goFileExt))
		f, err := os.Create(filePath)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = f.Write(buffer.Bytes()); err != nil {
			return err
		}
		index++
		buffer.Reset()
		setHeader = false
		return nil
	}
	for _, v := range models {
		if !setHeader {
			if err = setFileHeader(); err != nil {
				return err
			}
		}
		err = cTemplate.Execute(&buffer, Build.BuildTemplateMap(&v))
		size := buffer.Len()
		if size < maxSize {
			continue
		}
		if err = save(); err != nil {
			return err
		}
	}
	if buffer.Len() == 0 {
		return nil
	}
	return save()
}

func main() {
	initFlag()
	files := getModelFiles()
	objects := files.getModelObjects()
	models := make(modelInfos, len(objects))
	for i, obj := range objects {
		models[i] = buildModelInfo(obj)
	}
	if err := models.generateFile(); err != nil {
		panic(err)
	}
}

func initFlag() {
	flag.StringVar(&modelPath, "path", "./", "")
	flag.StringVar(&generateFilePrefix, "filePrefix", "auto_generate_", "")
	flag.Parse()
}

func isTablerFunc(decl *ast.FuncDecl) (*ast.Object, bool) {
	const funcName = "TableName"
	if len(decl.Recv.List) == 0 {
		return nil, false
	}
	if decl.Name.Name != funcName {
		return nil, false
	}
	if len(decl.Type.Params.List) > 0 {
		return nil, false
	}
	if len(decl.Type.Results.List) != 1 {
		return nil, false
	}
	if typ, ok := decl.Type.Results.List[0].Type.(*ast.Ident); !ok || typ.Name != _string {
		return nil, false
	}
	switch decl.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		typ := decl.Recv.List[0].Type.(*ast.StarExpr)
		if ident, ok := typ.X.(*ast.Ident); !ok {
			return nil, false
		} else {
			return ident.Obj, true
		}
	case *ast.Ident:
		typ := decl.Recv.List[0].Type.(*ast.Ident)
		return typ.Obj, true
	default:
		return nil, false
	}
}

func getModelFiles() (files fileInfos) {
	_ = filepath.Walk(modelPath, func(filePath string, info os.FileInfo, err error) error {
		if info.IsDir() || path.Ext(info.Name()) != _goFileExt {
			return nil
		}
		if strings.HasPrefix(info.Name(), generateFilePrefix) {
			_ = os.Remove(filePath)
			return nil
		}
		fast, _ := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.AllErrors)
		files = append(files, fast)
		return nil
	})
	return
}

func buildModelInfo(obj *ast.Object) (m ModelInfo) {
	m.Name = obj.Name
	objFields := obj.Decl.(*ast.TypeSpec).Type.(*ast.StructType).Fields.List
	m.Fields = make([]ModelFieldInfo, len(objFields))
	var primaryKey *ModelFieldInfo
	for i, filed := range objFields {
		m.Fields[i] = buildModelFieldInfo(filed)
		if m.Fields[i].PrimaryKey {
			primaryKey = &m.Fields[i]
			continue
		}
		if primaryKey == nil && m.Fields[i].Column == "id" {
			primaryKey = &m.Fields[i]
		}
	}
	if primaryKey != nil {
		primaryKey.PrimaryKey = true
	}
	return
}

func buildModelFieldInfo(af *ast.Field) (field ModelFieldInfo) {
	field.Name = af.Names[0].Name
	if af.Tag == nil || af.Tag.Value == "" {
		field.Column = snakeString(field.Name)
	} else {
		tag := reflect.StructTag(af.Tag.Value[1 : len(af.Tag.Value)-2])
		if tagValue := tag.Get("gorm"); tagValue != "" {
			for _, v := range strings.Split(tagValue, ";") {
				if strings.HasPrefix(v, _columnPrefix) {
					field.Column = strings.TrimPrefix(v, _columnPrefix)
				} else if v == _primaryKey1 || v == _primaryKey2 {
					field.PrimaryKey = true
				}
			}
		} else if tagValue = tag.Get("json"); tagValue != "" {
			field.Column = tagValue
		} else {
			field.Column = snakeString(field.Name)
		}
	}
	afType := af.Type
	starLoop := 0
	for {
		if _, ok := afType.(*ast.StarExpr); !ok {
			break
		}
		afType = afType.(*ast.StarExpr).X
		starLoop++
	}
	defer func() {
		for i := 0; i < starLoop; i++ {
			field.Type = "*" + field.Type
		}
	}()
	switch afType.(type) {
	case *ast.Ident:
		field.Type = afType.(*ast.Ident).Name
	case *ast.SelectorExpr:
		typ := afType.(*ast.SelectorExpr)
		field.Type = fmt.Sprintf("%s.%s", typ.X.(*ast.Ident).Name, typ.Sel.Name)
	}
	return
}

func snakeString(str string) string {
	const diff = 'a' - 'A'
	sb := strings.Builder{}
	for i, v := range []rune(strings.ReplaceAll(str, "ID", "id")) {
		if v >= 'A' && v <= 'Z' {
			if i != 0 {
				sb.WriteRune('_')
			}
			sb.WriteRune(v + diff)
		} else {
			sb.WriteRune(v)
		}
	}
	return sb.String()
}
