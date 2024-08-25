package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/samber/lo"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/settings"
	"golang.org/x/tools/internal/tool"
)

func (d *definition) Run(ctx context.Context, args ...string) error {
	if len(args) != 1 {
		return tool.CommandLineErrorf("definition expects 1 argument")
	}

	opts := d.app.options
	d.app.options = func(o *settings.Options) {
		if opts != nil {
			opts(o)
		}
		o.PreferredContentFormat = protocol.PlainText
		if d.MarkdownSupported {
			o.PreferredContentFormat = protocol.Markdown
		}
	}

	conn, err := d.app.connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.terminate(ctx)

	defs, err := d.getDefs(ctx, conn, args[0])
	if err != nil {
		return fmt.Errorf("failed to get definitions: %w", err)
	}

	for _, def := range defs {
		fmt.Println("------------------------------------------------")
		fmt.Println(def.Package)
		fmt.Println()
		fmt.Println(def.Description)
	}

	return nil
}

var (
	ErrEndOfLine       = errors.New("column is beyond end of line")
	ErrEndOfFile       = errors.New("column is beyond end of file")
	ErrNotAnIdentifier = errors.New("not an identifier")
)

type DDefinition struct {
	Span        span   `json:"span"`
	Description string `json:"description"`
	Name        string
	Begin       LineColumn
	End         LineColumn
	Package     string
	Filepath    string
}

type LineColumn struct {
	Row    uint32
	Column uint32
}

type DefLevels struct {
	defByLevel map[int64][]*DDefinition
}

func (d *DefLevels) AddToLevel(defs []*DDefinition, level int64) {
	if len(defs) == 0 {
		return
	}
	if d.defByLevel == nil {
		d.defByLevel = make(map[int64][]*DDefinition, 16)
	}
	d.defByLevel[level] = append(d.defByLevel[level], defs...)
}

func (d *DefLevels) GetByLevel(level int64) []*DDefinition {
	return d.defByLevel[level]
}

func (d *DefLevels) GetAllUniq() []*DDefinition {
	var resp []*DDefinition
	keys := lo.Keys(d.defByLevel)
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		resp = append(resp, d.defByLevel[key]...)
	}
	return lo.UniqBy(resp, func(x *DDefinition) string { return x.Description })
}

func (d *definition) getProcesedDefs(ctx context.Context, conn *connection, filepath string) ([]*DDefinition, error) {
	myDef, err := GetMyDef(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to get my definition: %w", err)
	}
	if myDef == nil {
		return nil, nil
	}

	from := parseSpan(filepath)
	file, err := conn.openFile(ctx, from.URI())
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	defs, err := d.GetDefsByStartEnd(
		ctx,
		file,
		conn,
		int(myDef.Begin.Row)+1,
		int(myDef.Begin.Column),
		int(myDef.End.Row)+1,
		int(myDef.End.Column),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get definitions by start and end: %w", err)
	}

	modValue, err := getLastModuleValue("./go.mod")
	if err != nil {
		return nil, fmt.Errorf("failed to get module value: %w", err)
	}

	processedDefs := lo.Filter(lo.UniqBy(defs, func(x *DDefinition) string { return x.Description }), func(x *DDefinition, i int) bool {
		return strings.Contains(x.Span.URI().Dir().Path(), modValue) &&
			!strings.HasPrefix(x.Description, "field") &&
			!strings.HasPrefix(x.Description, "package") &&
			(!strings.Contains(x.Span.URI().Dir().Path(), "/vendor/") || (strings.Contains(x.Span.URI().Dir().Path(), "/vendor/") && strings.Contains(x.Span.URI().Dir().Path(), "git.uzum.io"))) &&
			x.Description != ""
	})

	return lo.Filter(processedDefs, func(x *DDefinition, i int) bool {
		return myDef.Filepath != x.Span.URI().Path() || (int(myDef.Begin.Row) > x.Span.Start().Line() || int(myDef.End.Row) < x.Span.Start().Line())
	}), nil
}

func GetMyDef(filepath string) (*DDefinition, error) {
	parts := strings.Split(filepath, ":")
	actualPath := parts[0]
	lineNumber, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid line number: %w", err)
	}

	defStartStop, err := GetDefinitionsOfStartStop(actualPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get definitions of start and stop: %w", err)
	}

	for _, def := range defStartStop {
		if def.Begin.Row+1 <= uint32(lineNumber) && def.End.Row+1 >= uint32(lineNumber) {
			return def, nil
		}
	}

	return nil, nil
}

func (d *definition) GetDefsByStartEnd(
	ctx context.Context,
	file *cmdFile,
	conn *connection,
	startLine, startColumn, endLine, endColumn int,
) ([]*DDefinition, error) {
	var defs []*DDefinition

	// Extract words using getWordsWithOffsets
	words, err := getWordsWithOffsets(file.uri.Path(), startLine, startColumn, endLine, endColumn)
	if err != nil {
		return nil, err
	}

	// Iterate over each word and get definitions
	for _, word := range words {
		cleanPath := fmt.Sprintf("%s:%d:%d", file.uri.Path(), word.Line, word.Column)
		fromSpan := parseSpan(cleanPath)
		def, err := d.GetDefinition(ctx, file, fromSpan, conn)
		if err != nil {
			if strings.Contains(err.Error(), ErrEndOfLine.Error()) {
				continue
			}
			if strings.Contains(err.Error(), ErrEndOfFile.Error()) {
				break
			}
			continue
		}
		defs = append(defs, def)
	}

	return defs, nil
}

// getLastModuleValue parses the ./go.mod file and returns the last value of the module path.
func getLastModuleValue(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("could not open file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Trim leading and trailing whitespace
		line = strings.TrimSpace(line)
		// Check if the line starts with "module"
		if strings.HasPrefix(line, "module") {
			// Split the line by spaces
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return "", fmt.Errorf("invalid module line: %s", line)
			}
			modulePath := parts[1]
			// Split the module path by "/"
			segments := strings.Split(modulePath, "/")
			if len(segments) == 0 {
				return "", fmt.Errorf("invalid module path: %s", modulePath)
			}
			// Return the last segment
			return segments[len(segments)-1], nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}

	return "", fmt.Errorf("no module directive found in %s", filePath)
}

type Word struct {
	Line   int
	Column int
	Text   string
}

func isWordBoundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(".,;:!?\"'()[]{}=0123456789", r)
}

func getWordsWithOffsets(filePath string, startLine, startColumn, endLine, endColumn int) ([]Word, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var words []Word
	lineNumber := 1

	for scanner.Scan() {
		if lineNumber >= startLine && lineNumber <= endLine {
			line := scanner.Text()
			start := 0
			end := len(line)

			if lineNumber == startLine {
				start = startColumn - 1
			}
			if lineNumber == endLine {
				end = endColumn
			}

			if start < 0 {
				start = 0
			}
			if end > len(line) {
				end = len(line)
			}

			lineSegment := line[start:end]
			wordStart := -1
			for i, r := range lineSegment {
				if isWordBoundary(r) {
					if wordStart != -1 {
						words = append(words, Word{
							Line:   lineNumber,
							Column: start + wordStart + 1,
							Text:   lineSegment[wordStart:i],
						})
						wordStart = -1
					}
				} else {
					if wordStart == -1 {
						wordStart = i
					}
				}
			}
			if wordStart != -1 {
				words = append(words, Word{
					Line:   lineNumber,
					Column: start + wordStart + 1,
					Text:   lineSegment[wordStart:],
				})
			}
		}
		lineNumber++
		if lineNumber > endLine {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return words, nil
}

func (d *definition) GetDefinition(ctx context.Context, file *cmdFile, from span, conn *connection) (*DDefinition, error) {
	loc, err := file.spanLocation(from)
	if err != nil {
		return nil, fmt.Errorf("failed to get location for %s: %w", from, err)
	}
	p := protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.LocationTextDocumentPositionParams(loc),
	}
	locs, err := conn.Definition(ctx, &p)
	if err != nil {
		return nil, fmt.Errorf("failed to get definition: %w", err)
	}

	if len(locs) == 0 {
		return nil, fmt.Errorf("no locations found for %v: %w", from, ErrNotAnIdentifier)
	}

	file, err = conn.openFile(ctx, locs[0].URI)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	definition, err := file.locationSpan(locs[0])
	if err != nil {
		return nil, fmt.Errorf("failed to get location span: %w", err)
	}

	q := protocol.HoverParams{
		TextDocumentPositionParams: protocol.LocationTextDocumentPositionParams(loc),
	}
	hover, err := conn.Hover(ctx, &q)
	if err != nil {
		return nil, fmt.Errorf("failed to get hover info: %w", err)
	}

	description := ""
	if hover != nil {
		description = strings.TrimSpace(hover.Contents.Value)
	}

	result := &DDefinition{
		Span:        definition,
		Description: description,
	}

	if d.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "\t")
		return nil, fmt.Errorf("failed to encode result: %w", enc.Encode(result))
	}

	return result, nil
}

func (d *definition) getDefs(ctx context.Context, conn *connection, filepath string) ([]*DDefinition, error) {
	defLevel := DefLevels{}
	processedDefs, err := d.getProcesedDefs(ctx, conn, filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to get processed definitions: %w", err)
	}

	processedDefs, err = enhanceDef(processedDefs)
	if err != nil {
		return nil, fmt.Errorf("failed to enhance definitions: %w", err)
	}

	level := int64(1)
	def, err := GetMyDef(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to get my definition: %w", err)
	}
	if def == nil {
		return nil, nil
	}

	defLevel.AddToLevel([]*DDefinition{{
		Description: def.Description,
		Name:        def.Name,
		Begin:       def.Begin,
		End:         def.End,
		Package:     def.Package,
	}}, 0)

	defLevel.AddToLevel(processedDefs, level)

	for {
		level++
		if level >= 3 {
			break
		}

		toProcessDefs := defLevel.GetByLevel(level - 1)
		if len(toProcessDefs) == 0 {
			break
		}
		if len(toProcessDefs) >= 15 {
			fmt.Printf("too many definitions found %v, level: %v\n", len(toProcessDefs), level)
			break
		}
		for _, def := range toProcessDefs {
			anotherLevels, err := d.getProcesedDefs(ctx, conn, def.Span.URI().Path()+":"+strconv.Itoa(def.Span.Start().Line()))
			if err != nil {
				return nil, fmt.Errorf("failed to get processed definitions for another level: %w", err)
			}

			anotherLevels, err = enhanceDef(anotherLevels)
			if err != nil {
				return nil, fmt.Errorf("failed to enhance definitions for another level: %w", err)
			}

			defLevel.AddToLevel(anotherLevels, level)
		}
	}

	resp := defLevel.GetAllUniq()
	for _, res := range resp {
		res.Description = removeComments(res.Description)
	}

	return resp, nil
}

func removeComments(src string) string {
	var result []string
	lines := strings.Split(src, "\n")
	for _, line := range lines {
		if index := strings.Index(line, "//"); index != -1 {
			line = line[:index]
		}
		if trimmedLine := strings.TrimSpace(line); trimmedLine != "" {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

func enhanceDef(defs []*DDefinition) ([]*DDefinition, error) {
	if len(defs) == 0 {
		return nil, nil
	}

	var resp []*DDefinition

	for _, def := range defs {
		if def == nil {
			continue
		}
		respDef, err := GetMyDef(def.Span.URI().Path() + ":" + strconv.Itoa(def.Span.Start().Line()))
		if err != nil {
			return nil, fmt.Errorf("failed to get my definition: %w", err)
		}
		if respDef == nil {
			continue
		}
		if respDef.Description == "" || strings.HasPrefix(respDef.Description, "unreadable: ") {
			continue
		}

		def.Description = respDef.Description
		def.Name = respDef.Name
		def.Begin = respDef.Begin
		def.End = respDef.End
		def.Package = respDef.Package
		resp = append(resp, def)
	}

	return resp, nil
}

func GetDefinitionsOfStartStop(filepath string) ([]*DDefinition, error) {
	var actualPath string
	if strings.Contains(filepath, ":") {
		parts := strings.Split(filepath, ":")
		actualPath = parts[0]
	} else {
		actualPath = filepath
	}

	sourceCode, err := os.ReadFile(actualPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read source code file: %w", err)
	}

	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(golang.GetLanguage())

	tree, err := parser.ParseCtx(context.Background(), nil, sourceCode)
	if err != nil {
		return nil, fmt.Errorf("failed to parse source code: %w", err)
	}
	defer tree.Close()

	queries := map[string]string{
		"method":     MethodDeclaration,
		"function":   FunctionDeclaration,
		"typeStruct": TypeStructDeclaration,
		"interface":  InterfaceDeclaration,
		"typeAlias":  TypeAliasDeclaration,
		"var":        VarDeclarationSourceFile,
		"const":      ConstDeclaration,
	}

	var results []*DDefinition

	for queryName, query := range queries {
		query, err := sitter.NewQuery([]byte(query), golang.GetLanguage())
		if err != nil {
			return nil, fmt.Errorf("failed to create new query: %w", err)
		}
		queryCursor := sitter.NewQueryCursor()
		queryCursor.Exec(query, tree.RootNode())

		for {
			match, ok := queryCursor.NextMatch()
			if !ok {
				break
			}
			begin := LineColumn{
				Row:    match.Captures[1].Node.StartPoint().Row,
				Column: match.Captures[1].Node.StartPoint().Column,
			}
			end := LineColumn{
				match.Captures[1].Node.EndPoint().Row,
				match.Captures[1].Node.EndPoint().Column,
			}
			packageName := match.Captures[0].Node.Content(sourceCode)
			definition := match.Captures[1].Node.Content(sourceCode)
			name := match.Captures[2].Node.Content(sourceCode)
			def := &DDefinition{
				Filepath:    filepath,
				Name:        name,
				Description: definition,
				Begin:       begin,
				End:         end,
				Package:     packageName,
			}
			if queryName == "const" {
				def.Description = "const " + def.Description
			}
			results = append(results, def)
		}
	}

	return results, nil
}

var (
	MethodDeclaration = `
(source_file
  (package_clause) @package_clause
  (method_declaration
    name: (field_identifier) @field.identifier
  ) @method.declaration
)
`

	FunctionDeclaration = `
(source_file
  (package_clause) @package_clause
  (function_declaration
    name: (identifier) @identifier
  ) @function.declaration
)
`

	TypeStructDeclaration = `
(source_file
  (package_clause) @package_clause
  (type_declaration
    (type_spec
      name: (type_identifier) @type.name
      type: (struct_type)
    )
  ) @type.struct.declaration
)
`

	InterfaceDeclaration = `
(source_file
  (package_clause) @package_clause
  (type_declaration
    (type_spec
      name: (type_identifier)
      type: (interface_type
        (method_spec) @interface.method.declaration
      )
    )
  ) @type.interface.declaration
)
`

	TypeAliasDeclaration = `
(source_file
  (package_clause) @package_clause
  (type_declaration
    (type_spec
      name: (type_identifier) @type.alias.name
      type: (type_identifier) @type.alias.type
    )
  ) @type.alias.declaration
)
`

	VarDeclarationSourceFile = `
(source_file
  (package_clause) @package_clause
  (var_declaration
    (var_spec
      (identifier) @var.identifier
    )
  ) @var.declaration
)
`

	ConstDeclaration = `
(source_file
  (package_clause) @package_clause
  (const_declaration
    (const_spec
      name: (identifier) @const.identifier
    ) @const.declaration
  )
)
`
)
