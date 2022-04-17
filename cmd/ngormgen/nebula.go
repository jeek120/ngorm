package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"golang.org/x/tools/go/packages"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

const POTYPE_TAG = "Tag"
const POTYPE_EDGE = "Edge"

var IDFIELD = &Field{
	name:     "Id",
	nickname: "id",
	typeStr:  "int64",
	comment:  "",
}

var (
	typeNames   = flag.String("type", "", "comma-separated list of type names; must be set")
	output      = flag.String("output", "", "output file name; default srcdir/<type>_string.go")
	trimprefix  = flag.String("trimprefix", "", "trim the `prefix` from the generated constant names")
	linecomment = flag.Bool("linecomment", false, "use line comment text as printed text when present")
	buildTags   = flag.String("tags", "", "comma-separated list of build tags to apply")
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of ngormgen:\n")
	fmt.Fprintf(os.Stderr, "\tngormgen [flags] -type T [directory]\n")
	fmt.Fprintf(os.Stderr, "\tngormgen [flags] -type T files... # Must be a single package\n")
	fmt.Fprintf(os.Stderr, "For more information, see:\n")
	fmt.Fprintf(os.Stderr, "\thttps://github.com/jeek120/ngorm/cmd/ngormgen\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("ngormgen: ")
	flag.Usage = Usage
	flag.Parse()
	var tags []string
	if len(*buildTags) > 0 {
		tags = strings.Split(*buildTags, ",")
	}

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	g := Generator{
		trimPrefix:  *trimprefix,
		lineComment: *linecomment,
	}
	// TODO(suzmue): accept other patterns for packages (directories, list of files, import paths, etc).
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
	} else {
		if len(tags) != 0 {
			log.Fatal("-tags option applies only to directories, not when files are specified")
		}
		dir = filepath.Dir(args[0])
	}

	g.parsePackage(args, tags)

	// Print the header and package clause.
	g.Printf("// Code generated by \"ngormgen %s\"; DO NOT EDIT.\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")
	g.Printf("package %s", g.pkg.name)
	g.Printf("\n")
	g.Printf(`import (`) // Used by all methods.
	// g.Printlnf(`	"github.com/jeek120/ngorm/util"`)
	g.Printlnf(`	"strconv"`)
	g.Printlnf(`	nebula_go "github.com/vesoft-inc/nebula-go/v3"`)
	g.Printlnf(`		"strings"`)
	g.Printlnf(`		"fmt"`)
	g.Printlnf(`)`)

	// Run generate for each type.
	var types []string
	if len(*typeNames) > 0 {
		types = strings.Split(*typeNames, ",")
	}
	g.generate(dir, types)

	// Format the output.
	src := g.format()

	// Write to file.
	outputName := *output
	if outputName == "" {
		baseName := "ngorm_generate.go"
		outputName = filepath.Join(dir, strings.ToLower(baseName))
	}
	err := ioutil.WriteFile(outputName, src, 0644)
	if err != nil {
		log.Fatalf("writing output: %s", err)
	}
}

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf bytes.Buffer // Accumulated output.
	pkg *Package     // Package we are scanning.

	Structs     []Struct
	trimPrefix  string
	lineComment bool
}

func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

func (g *Generator) Printlnf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format+"\n", args...)
}

// File holds a single parsed file and associated data.
type File struct {
	dir            string
	pkg            *Package       // Package to which this file belongs.
	file           *ast.File      // Parsed AST.
	allowTypeNames map[string]int // 容许的类型名称，为nil则为全部
	structs        []Struct
	trimPrefix     string
	lineComment    bool
}

type Struct struct {
	// These fields are reset for each type being generated.
	name     string // Name of the constant type.
	nickname string
	fields   []Field // Accumulator for constant fields of that type.
	isTag    bool
	isEdge   bool
}

type Package struct {
	name  string
	defs  map[*ast.Ident]types.Object
	files []*File
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *Generator) parsePackage(patterns []string, tags []string) {
	cfg := &packages.Config{
		Mode: packages.LoadSyntax,
		// TODO: Need to think about constants in test files. Maybe write type_string_test.go
		// in a separate pass? For later.
		Tests:      false,
		BuildFlags: []string{fmt.Sprintf("-tags=%s", strings.Join(tags, " "))},
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *Generator) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name:  pkg.Name,
		defs:  pkg.TypesInfo.Defs,
		files: make([]*File, 0),
	}

	for _, file := range pkg.Syntax {
		if len(file.Comments) > 0 {
			if strings.Contains(file.Comments[0].Text(), "Code generated by") {
				continue
			}
		}

		g.pkg.files = append(g.pkg.files, &File{
			file:        file,
			pkg:         g.pkg,
			trimPrefix:  g.trimPrefix,
			lineComment: g.lineComment,
			structs:     make([]Struct, 0),
		})
	}
}

// generate produces the String method for the named type.
func (g *Generator) generate(dir string, allowTypeNames []string) {
	for _, file := range g.pkg.files {
		file.dir = dir
		// Set the state for this run of the walker.
		if len(allowTypeNames) > 0 {
			file.allowTypeNames = make(map[string]int, 0)
			for _, allowTypeName := range allowTypeNames {
				file.allowTypeNames[allowTypeName] = 1
			}
		}
		if file.file != nil {
			ast.Inspect(file.file, file.genStruct)
		}
		if g.Structs == nil {
			g.Structs = file.structs
		} else {
			g.Structs = append(g.Structs, file.structs...)
		}
	}

	g.checkResultSet()
	for _, s := range g.Structs {
		g.funcAllFields(&s)
		g.funcAllFieldsWithId(&s)
		g.funcTagName(&s)
		g.funcEdgeName(&s)
		g.funcNqlNameValues(&s)
		g.funcNqlValues(&s)
		g.funcNqlNames(&s)
		g.funcNqlBind(&s)

		// 创建语句
		g.CreateTag(&s)
		g.CreateEdge(&s)

		// 插入
		g.funcInsertTag(&s)
		g.funcInsertEdge(&s)

		// 查询
		g.funcBindRecord(&s)
		g.funcBindResult(&s)
		g.funcConditionItem(&s)
		g.funcBindOne(&s)
		g.funcOne(&s)
		g.funcList(&s)

		// 删除
		g.funcRemoveTag(&s)
		g.funcRemoveEdge(&s)
	}
	g.Create()
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *Generator) format() []byte {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		// The user can compile the output to see the error.
		log.Printf("warning: internal error: invalid Go generated: %s", err)
		log.Printf("warning: compile the package to analyze the error")
		return g.buf.Bytes()
	}
	return src
}

// Field represents a declared constant.
type Field struct {
	name     string
	nickname string
	// The value is stored as a bit pattern alone. The boolean tells us
	// whether to interpret it as an int64 or a uint64; the only place
	// this matters is when sorting.
	// Much of the time the str field is all we need; it is printed
	// by Field.String.
	typeStr          string // The string representation given by the "go/constant" package.
	comment          string
	isIndex          bool
	otherIndexFields string
}

func (v *Field) String() string {
	return v.name + " " + v.typeStr
}

func (f *File) allowTypeName(name string) bool {
	if f.allowTypeNames == nil {
		return true
	}

	_, exist := f.allowTypeNames[name]
	return exist
}

// genStruct processes one declaration clause.
func (f *File) genStruct(node ast.Node) bool {
	fmt.Println(node)
	if fd, ok := node.(*ast.FuncDecl); ok {
		for _, l := range fd.Recv.List {
			if s, ok := l.Type.(*ast.SelectorExpr); ok {
				println(s)
			}
			if s, ok := l.Type.(*ast.StarExpr); ok {
				if id, ok := s.X.(*ast.Ident); ok {
					println(id)
				}
			}
		}
	}
	s, ok := node.(*ast.TypeSpec)
	if ok {
		if !f.allowTypeName(s.Name.Name) {
			return true
		}
		if st, ok2 := s.Type.(*ast.StructType); ok2 {
			stru := Struct{
				name:     s.Name.Name,
				nickname: strings.ToLower(s.Name.Name),
			}
			stru.fields = make([]Field, 0)
			for _, field := range st.Fields.List {

				if fieldType, ok3 := field.Type.(*ast.Ident); ok3 {
					for _, name := range field.Names {
						fi := Field{name: name.Name, nickname: strings.ToLower(name.Name), typeStr: fieldType.Name, comment: strings.TrimSpace(field.Comment.Text())}
						if field.Tag != nil {
							fi.otherIndexFields, fi.isIndex = reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Lookup("idx")
						}
						stru.fields = append(stru.fields, fi)
					}
				} else if fieldType, ok := field.Type.(*ast.SelectorExpr); ok {
					if fieldType.Sel.Name == POTYPE_TAG {
						stru.isTag = true
					} else if fieldType.Sel.Name == POTYPE_EDGE {
						stru.isEdge = true
					}
				} else if fieldType, ok := field.Type.(*ast.StarExpr); ok {
					if fieldType, ok := fieldType.X.(*ast.SelectorExpr); ok {
						if fieldType.Sel.Name == POTYPE_TAG {
							stru.isTag = true
							// stru.fields = append(stru.fields, Field{name: "Id", nickname: "id", typeStr: "int64", comment: ""})
						} else if fieldType.Sel.Name == POTYPE_EDGE {
							stru.isEdge = true
						}
					}
				}
			}
			f.structs = append(f.structs, stru)
			fmt.Println(st)
		}
		if len(f.allowTypeNames) == 1 {
			return false
		}
		delete(f.allowTypeNames, s.Name.Name)
	}
	return true
}

// help

func (f *Field) toNebulaType() string {
	return f.typeStr
}

func (f *Field) funcBindResult(struct_name, prefix string) string {
	var val string
	var set string
	if f.nickname == IDFIELD.nickname {
		set = struct_name + `.SetId(f)`
	} else {
		set = struct_name + `.` + f.name + ` = ` + f.typeStr + `(f)`
	}
	if f.typeStr == "string" {
		val = "AsString()"
	} else if f.typeStr == "int" {
		val = "AsInt()"
	} else if f.typeStr == "int64" {
		val = "AsInt()"
	} else if f.typeStr == "int32" {
		val = "AsInt()"
	} else if f.typeStr == "int16" {
		val = "AsInt()"
	} else if f.typeStr == "int8" {
		val = "AsInt()"
	} else if f.typeStr == "float64" {
		val = "AsFloat()"
	} else if f.typeStr == "float32" {
		val = "AsFloat()"
	} else {
		panic(f.typeStr + "unsupport")
	}

	return `
val,err := record.GetValueByColName("` + prefix + f.nickname + `")
			if err != nil {
				panic(err)
			}
			f,err := val.` + val + `
			if err != nil {
				panic(err)
			}
` + set
}

func (f *Field) funcValue(structName string) string {
	if f.typeStr == "string" {
		return `"\"" + ` + structName + `.` + f.name + ` + "\""`
	} else if f.typeStr == "int" {
		return `strconv.Itoa(` + structName + `.` + f.name + `)`
	} else if f.typeStr == "int64" {
		return `strconv.FormatInt(` + structName + `.` + f.name + `, 10)`
	} else if f.typeStr == "int32" {
		return `strconv.FormatInt(int64(` + structName + `.` + f.name + `), 10)`
	} else if f.typeStr == "int16" {
		return `strconv.FormatInt(int64(` + structName + `.` + f.name + `), 10)`
	} else if f.typeStr == "int8" {
		return `strconv.FormatInt(int64(` + structName + `.` + f.name + `), 10)`
	} else if f.typeStr == "float64" {
		return `strconv.FormatFloat(` + structName + `.` + f.name + `, 'E', -1, 64)`
	} else if f.typeStr == "float32" {
		return `strconv.FormatFloat(float64(` + structName + `.` + f.name + `), 'E', -1, 32)`
	}
	panic(f.typeStr + "unsupport")
}

func (f *Field) funcEq(prefix string, structName string, nqlVarName string) string {
	if f.name == IDFIELD.name {
		return "id(" + nqlVarName + ")==" + f.funcValue(structName)
	}
	return "\"" + prefix + f.nickname + "==\"+" + f.funcValue(structName)
}

func (g *Generator) funcConditionItem(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) ConditionItem(fields ...string) []string {`)
	g.Printlnf(`result := make([]string, 0)`)
	if len(s.fields) > 0 {
		g.Printlnf("	for _, f := range fields {")
		for i, f := range s.fields {
			if i != 0 {
				g.Printf(`else `)
			}
			g.Printlnf(`if f == "` + f.nickname + `" {`)
			g.Printlnf(`result = append(result,` + f.funcEq("v."+s.nickname+".", "m", "v") + `)`)
			g.Printf("}")
		}
		g.Printlnf("\n	}")
	}
	g.Printlnf(`return result`)

	g.Printlnf("}")
}

func (g *Generator) funcBindOne(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) BindOne(result *nebula_go.ResultSet,fields ...string) {`)
	g.Printlnf(`if result.GetRowSize() == 0 {`)
	g.Printlnf(`	return`)
	g.Printlnf(`}`)
	g.Printlnf(`record,err := result.GetRowValuesByIndex(0)`)
	g.Printlnf(`if err != nil {`)
	g.Printlnf(`panic(err)`)
	g.Printlnf(`}`)
	g.Printlnf(`m.BindRecord(record)`)
	g.Printlnf(`}`)
}

func (g *Generator) funcOne(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) One(session *nebula_go.Session,fields ...string) {`)
	g.Printlnf(`var where string`)
	g.Printlnf(`if len(fields) > 0 {`)
	g.Printlnf(`where = " WHERE " + strings.Join(m.ConditionItem(fields...), ",")`)
	g.Printlnf(`}`)
	g.Printlnf(`nql := "MATCH (v:` + s.nickname + `) " + where + " return id(v) as ` + s.nickname + `_id" + `)
	g.Printlnf("`")
	for _, f := range s.fields {
		g.Printlnf(`	,v.` + s.nickname + `.` + f.nickname + ` as ` + s.nickname + `_` + f.nickname)
	}
	g.Printlnf(" limit 1`")
	g.Printlnf(`result,err := session.Execute(nql)`)
	g.Printlnf(`if err != nil {`)
	g.Printlnf(`panic(err)`)
	g.Printlnf(`}`)
	g.Printlnf(`if result.GetErrorCode() != 0 {`)
	g.Printlnf(`	panic(result.GetErrorMsg())`)
	g.Printlnf(`}`)
	g.Printlnf(`if result.GetRowSize() == 0 {`)
	g.Printlnf(`	return`)
	g.Printlnf(`}`)
	g.Printlnf(`record,err := result.GetRowValuesByIndex(0)`)
	g.Printlnf(`if err != nil {`)
	g.Printlnf(`panic(err)`)
	g.Printlnf(`}`)
	g.Printlnf(`m.BindRecord(record)`)
	g.Printlnf(`}`)
}

func (g *Generator) funcList(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) List(session *nebula_go.Session, ms *` + s.name + `List, offset, size int64, fields ...string) {`)
	g.Printlnf(`var where string`)
	g.Printlnf(`if len(fields) > 0 {`)
	g.Printlnf(`where = " WHERE " + strings.Join(m.ConditionItem(fields...), ",")`)
	g.Printlnf(`}`)
	g.Printlnf(`nql := "MATCH (v:` + s.nickname + `) " + where + " return id(v) as ` + s.nickname + `_id" +`)
	for _, f := range s.fields {
		g.Printlnf(`			",v.` + s.nickname + `.` + f.nickname + ` as ` + s.nickname + `_` + f.nickname + `" +`)
	}
	g.Printlnf(` " SKIP " + strconv.FormatInt(offset, 10) + " LIMIT " + strconv.FormatInt(size, 10)`)
	g.Printlnf(`result,err := session.Execute(nql)`)
	g.Printlnf(`if err != nil {`)
	g.Printlnf(`panic(err)`)
	g.Printlnf(`}`)
	g.Printlnf(`if result.GetErrorCode() != 0 {`)
	g.Printlnf(`	panic(result.GetErrorMsg())`)
	g.Printlnf(`}`)
	g.Printlnf(`ms.BindResult(result)`)
	g.Printlnf(`}`)
}

func (g *Generator) funcInsertTag(s *Struct) {
	if !s.isTag {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) Insert(session *nebula_go.Session, fields ...string) {`)
	g.Printlnf(`if len(fields) == 0 {`)
	g.Printlnf(`fields = m.AllFields()`)
	g.Printlnf(`}`)
	g.Printlnf(`nql := "insert VERTEX " + m.TagName() +"("+m.NqlNames(fields...)+") VALUES " + 
	strconv.FormatInt(m.Id2(),10) + ":(" + m.NqlValues(fields...)+ ")"`)
	g.Printlnf(`result,_ := session.Execute(nql)`)
	g.Printlnf(`checkResultSet(nql, result)`)
	g.Printlnf("}")
}
func (g *Generator) funcInsertEdge(s *Struct) {
	if !s.isEdge {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) Insert(session *nebula_go.Session, fields ...string) {`)
	g.Printlnf(`if len(fields) == 0 {`)
	g.Printlnf(`fields = m.AllFields()`)
	g.Printlnf(`}`)
	g.Printlnf(`nql := "insert EDGE " + m.EdgeName() +"("+m.NqlNames(fields...)+") VALUES " + 
	strconv.FormatInt(m.Src(),10) + "->" + strconv.FormatInt(m.Dst(),10) + ":(" + m.NqlValues(fields...)+ ")"`)
	g.Printlnf(`result,_ := session.Execute(nql)`)
	g.Printlnf(`checkResultSet(nql, result)`)
	g.Printlnf("}")
}

func (g *Generator) funcRemoveTag(s *Struct) {
	if !s.isTag {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) RemoveById(session *nebula_go.Session) {`)
	g.Printlnf(`nql := "DELETE VERTEX " + strconv.FormatInt(m.Id(),10) + " WITH EDGE;"`)
	g.Printlnf(`result,_ := session.Execute(nql)`)
	g.Printlnf(`checkResultSet(nql, result)`)
	g.Printlnf("}")
}

func (g *Generator) funcRemoveEdge(s *Struct) {
	if !s.isEdge {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) RemoveById(session *nebula_go.Session) {`)
	g.Printlnf(`nql := "DELETE EDGE " + strconv.FormatInt(m.Src(),10) + "->" + strconv.FormatInt(m.Dst(),10)`)
	g.Printlnf(`result,_ := session.Execute(nql)`)
	g.Printlnf(`checkResultSet(nql, result)`)
	g.Printlnf("}")
}

func (g *Generator) funcBindResult(s *Struct) {
	g.Printlnf(`type ` + s.name + `List []*` + s.name)
	g.Printlnf(`func (ms *` + s.name + `List) BindResult(result *nebula_go.ResultSet, fields ...string) {`)
	g.Printlnf(`if len(fields) == 0 {`)
	g.Printlnf(`fields = (&` + s.name + `{}).AllFieldsWithId()`)
	g.Printlnf(`}`)

	g.Printlnf(`for i,_ := range result.GetRows() {`)
	g.Printlnf(`record,err := result.GetRowValuesByIndex(i)`)
	g.Printlnf(`if err != nil {`)
	g.Printlnf(`panic(err)`)
	g.Printlnf(`}`)
	g.Printlnf(`m := &` + s.name + `{}`)
	g.Printlnf(`m.BindRecord(record, fields...)`)
	g.Printlnf(`*ms = append(*ms, m)`)
	g.Printlnf("}")
	g.Printlnf("}")
}

func (g *Generator) funcBindRecord(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) BindRecord(record *nebula_go.Record, fields ...string) {`)
	g.Printlnf(`if len(fields) == 0 {`)
	g.Printlnf(`fields = m.AllFieldsWithId()`)
	g.Printlnf(`}`)
	if s.isTag {
		g.Printlnf(IDFIELD.funcBindResult("m", s.nickname+"_"))
	}
	if len(s.fields) > 0 {
		g.Printlnf("	for _, f := range fields {")
		for i, f := range s.fields {
			if i != 0 {
				g.Printf(`else `)
			}
			g.Printlnf(`if f == "` + f.nickname + `" {`)
			g.Printlnf(f.funcBindResult("m", s.nickname+"_"))
			g.Printf(`}`)
		}
		g.Printlnf("\n	}")
	}

	g.Printlnf("}")
}

func (g *Generator) funcAllFields(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) AllFields() []string {`)
	g.Printlnf(`	return []string{`)
	for _, f := range s.fields {
		g.Printf(`		"` + f.nickname + `",`)
	}
	g.Printlnf(`	}`)
	g.Printlnf(`}`)
}

func (g *Generator) funcAllFieldsWithId(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) AllFieldsWithId() []string {`)
	g.Printlnf(`	return []string{`)
	for _, f := range s.fields {
		g.Printf(`		"` + f.nickname + `",`)
	}
	g.Printf(`		"id",`)
	g.Printlnf(`	}`)
	g.Printlnf(`}`)
}

func (g *Generator) funcTagName(s *Struct) {
	if !s.isTag {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) TagName() string {`)
	g.Printlnf(`	return "` + s.nickname + `"`)
	g.Printlnf(`}`)
}

func (g *Generator) funcEdgeName(s *Struct) {
	if !s.isEdge {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) EdgeName() string {`)
	g.Printlnf(`	return "` + s.nickname + `"`)
	g.Printlnf(`}`)
}

func (g *Generator) funcNqlNames(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) NqlNames(fields ...string) string {`)
	g.Printlnf(`	return strings.Join(fields, ",")`)
	g.Printlnf(`}`)
}

func (g *Generator) funcNqlNameValues(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) NqlNameValues(split string, fields ...string) []string {`)
	g.Printlnf("	values := make([]string, 0)")
	if len(s.fields) > 0 {
		g.Printlnf("	for _, f := range fields {")
		for i, f := range s.fields {
			if i != 0 {
				g.Printf(`else `)
			}
			g.Printlnf(`if f == "` + f.nickname + `" {`)
			g.Printlnf(`values = append(values, "` + f.nickname + `" + split + ` + f.funcValue("m") + `)`)
			g.Printf("}")
		}
		g.Printlnf("\n	}")
	}

	g.Printlnf(`	return values`)
	g.Printlnf(`}`)
}

func (g *Generator) funcNqlBind(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) NqlBind(structName string, fields ...string) []string {`)
	g.Printlnf("	values := make([]string, 0)")
	if len(s.fields) > 0 {
		g.Printlnf("	for _, f := range fields {")
		for i, f := range s.fields {
			if i != 0 {
				g.Printf(`else `)
			}
			g.Printlnf(`if f == "` + f.nickname + `" {`)
			g.Printlnf(`values = append(values, structName + ".` + s.nickname + `.` + f.nickname + ` as ` + s.nickname + "_" + f.nickname + `")`)
			g.Printf("}")
		}
		g.Printlnf("\n	}")
	}

	g.Printlnf(`	return values`)
	g.Printlnf(`}`)
}

func (g *Generator) funcNqlValues(s *Struct) {
	g.Printlnf(`func (m *` + s.name + `) NqlValues(fields ...string) string {`)
	g.Printlnf("	var values string")
	if len(s.fields) > 0 {
		g.Printlnf("	for _, f := range fields {")
		for i, f := range s.fields {
			if i != 0 {
				g.Printf(`else `)
			}
			g.Printlnf(`if f == "` + f.nickname + `" {`)
			g.Printlnf(`values = values + "," + ` + f.funcValue("m"))
			g.Printf("}")
		}
		g.Printlnf("\n	}")
	}

	g.Printlnf(`	return values[1:]`)
	g.Printlnf(`}`)
}

func (g *Generator) Create() {
	g.Printlnf(`func Create(session *nebula_go.Session) {`)
	for _, s := range g.Structs {
		if s.isTag || s.isEdge {
			g.Printlnf(`(&` + s.name + `{}).Create(session)`)
		}
	}
	g.Printlnf(`}`)
}

// 创建
func (g *Generator) CreateTag(s *Struct) {
	if !s.isTag {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) Create(session *nebula_go.Session) {`)
	g.Printlnf("	nql:=`CREATE TAG IF NOT EXISTS ` + m.TagName() + `(")
	for i, f := range s.fields {
		g.Printf("		" + f.nickname + "			" + f.toNebulaType() + "			COMMENT '" + f.comment + "'")
		if i != len(s.fields)-1 {
			g.Printlnf(",")
		}
	}
	g.Printlnf(");`")
	g.Printlnf(`result,_ := session.Execute(nql)`)
	g.Printlnf(`checkResultSet(nql, result)`)

	for _, f := range s.fields {
		if f.isIndex {
			g.Printf(`nql = "CREATE TAG INDEX IF NOT EXISTS idx_` + f.nickname)
			g.Printlnf(` ON " + m.TagName() + "(` + f.otherIndexFields + `)"`)

			g.Printlnf(`result,_ = session.Execute(nql)`)
			g.Printlnf(`checkResultSet(nql, result)`)
		}
	}
	g.Printlnf("}")
}
func (g *Generator) CreateEdge(s *Struct) {
	if !s.isEdge {
		return
	}
	g.Printlnf(`func (m *` + s.name + `) Create(session *nebula_go.Session) {`)
	g.Printlnf("	nql := `CREATE EDGE IF NOT EXISTS ` + m.EdgeName() + `(")
	for i, f := range s.fields {
		g.Printf("		" + f.nickname + "			" + f.toNebulaType() + "			COMMENT '" + f.comment + "'")
		if i != len(s.fields)-1 {
			g.Printlnf(",")
		}
	}
	g.Printlnf("	);`")
	g.Printlnf(`result,_ := session.Execute(nql)`)
	g.Printlnf(`checkResultSet(nql, result)`)
	g.Printlnf("}")
}

func (g *Generator) checkResultSet() {
	g.Printlnf("%s", `
	func checkResultSet(prefix string, res *nebula_go.ResultSet) {
		if !res.IsSucceed() {
			panic(fmt.Sprintf("%s, ErrorCode: %v, ErrorMsg: %s", prefix, res.GetErrorCode(), res.GetErrorMsg()))
		}
	}`)
}
