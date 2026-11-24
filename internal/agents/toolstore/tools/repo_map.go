package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kayushkin/tool-store/schema"
)

// RepoMap returns a tool that generates a structural map of a codebase.
// For Go files: function signatures, types, structs, interfaces (compact format).
// For other files: filename and size.
func RepoMap(rootDir string, ignorePatterns []string) Impl {
	return Impl{
		Name:        "repo_map",
		Description: "Generate a structural map of the codebase showing packages, functions, types, and file organization. Use this to understand the project structure without reading full files.",
		InputSchema: schema.Props(
			nil, // no required fields
			map[string]any{
				"path":   schema.Str("Subdirectory to map (relative to repo root). Leave empty for entire repo."),
				"format": schema.Str("Output format: 'compact' (default, abbreviated) or 'full' (complete signatures)."),
			},
		),
		Run: func(ctx context.Context, input string) (string, error) {
			var params struct {
				Path   string `json:"path"`
				Format string `json:"format"`
			}
			if err := json.Unmarshal([]byte(input), &params); err != nil {
				return "", err
			}

			// Default to compact format
			if params.Format == "" {
				params.Format = "compact"
			}

			// Determine target directory
			targetDir := rootDir
			if params.Path != "" {
				targetDir = filepath.Join(rootDir, params.Path)
			}

			// Build the repo map
			result, err := buildRepoMap(targetDir, ignorePatterns, params.Format == "compact")
			if err != nil {
				return "", fmt.Errorf("failed to build repo map: %w", err)
			}

			return result, nil
		},
	}
}

// buildRepoMap generates a structural summary of the codebase
func buildRepoMap(rootDir string, ignorePatterns []string, compact bool) (string, error) {
	type entry struct {
		FilePath string
		Content  string
		IsGoFile bool
	}

	var entries []entry

	// Walk the directory
	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			baseName := filepath.Base(path)
			if baseName == ".git" || baseName == "node_modules" ||
				baseName == "vendor" || baseName == ".openclaw" ||
				baseName == "logs" {
				return filepath.SkipDir
			}
			return nil
		}

		// Get relative path
		relPath, _ := filepath.Rel(rootDir, path)
		if shouldIgnore(relPath, ignorePatterns) {
			return nil
		}

		// Process Go files with AST parsing
		if strings.HasSuffix(path, ".go") {
			var summary string
			var err error
			if compact {
				summary, err = parseGoFileCompact(path, relPath)
			} else {
				summary, err = parseGoFileFull(path, relPath)
			}

			if err != nil {
				// If parsing fails, still include the file name
				entries = append(entries, entry{
					FilePath: relPath,
					Content:  fmt.Sprintf("// Parse error: %v", err),
					IsGoFile: true,
				})
				return nil
			}

			if summary != "" {
				entries = append(entries, entry{
					FilePath: relPath,
					Content:  summary,
					IsGoFile: true,
				})
			}
		} else if isRelevantFile(path) {
			// For non-Go files, just show filename and size
			entries = append(entries, entry{
				FilePath: relPath,
				Content:  fmt.Sprintf("// %s (%d bytes)", filepath.Base(path), info.Size()),
				IsGoFile: false,
			})
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	// Sort entries by path
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].FilePath < entries[j].FilePath
	})

	// Build output
	var builder strings.Builder
	builder.WriteString("# Repository Structure\n\n")

	for _, e := range entries {
		builder.WriteString(fmt.Sprintf("## %s\n", e.FilePath))
		builder.WriteString(e.Content)
		builder.WriteString("\n\n")
	}

	return builder.String(), nil
}

// parseGoFileCompact extracts structure in compact format (30-50% smaller)
func parseGoFileCompact(filePath, relPath string) (string, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
	if err != nil {
		return "", err
	}

	var parts []string

	// Package
	if node.Name != nil {
		parts = append(parts, fmt.Sprintf("pkg %s", node.Name.Name))
	}

	// Imports - only third-party
	var importantImports []string
	for _, imp := range node.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, ".") {
			importantImports = append(importantImports, path)
		}
	}
	if len(importantImports) > 0 {
		parts = append(parts, fmt.Sprintf("imports: %s", strings.Join(importantImports, ", ")))
	}

	// Functions and methods
	for _, decl := range node.Decls {
		if funcDecl, ok := decl.(*ast.FuncDecl); ok {
			sig := compactFuncSignature(funcDecl)
			if sig != "" {
				parts = append(parts, sig)
			}
		}
	}

	// Types
	for _, decl := range node.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok {
			if genDecl.Tok == token.TYPE {
				for _, spec := range genDecl.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						typeStr := compactTypeDecl(typeSpec)
						if typeStr != "" {
							parts = append(parts, typeStr)
						}
					}
				}
			}
		}
	}

	if len(parts) <= 1 {
		return "", nil // Empty or package-only file
	}

	return strings.Join(parts, "\n"), nil
}

// parseGoFileFull extracts structure in full format
func parseGoFileFull(filePath, relPath string) (string, error) {
	// For full format, we can keep the existing verbose parser
	// For now, just use compact (can implement full later if needed)
	return parseGoFileCompact(filePath, relPath)
}

// Helper functions for compact parsing
func compactFuncSignature(decl *ast.FuncDecl) string {
	var sig strings.Builder

	// Receiver (method)
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		recvType := compactType(decl.Recv.List[0].Type)
		sig.WriteString(recvType)
		sig.WriteString(".")
	}

	sig.WriteString(decl.Name.Name)
	sig.WriteString("(")

	// Parameters (types only)
	if decl.Type.Params != nil {
		params := []string{}
		for _, param := range decl.Type.Params.List {
			pType := compactType(param.Type)
			count := len(param.Names)
			if count == 0 {
				count = 1
			}
			for i := 0; i < count; i++ {
				params = append(params, pType)
			}
		}
		sig.WriteString(strings.Join(params, ", "))
	}
	sig.WriteString(")")

	// Return types
	if decl.Type.Results != nil && len(decl.Type.Results.List) > 0 {
		results := []string{}
		for _, result := range decl.Type.Results.List {
			rType := compactType(result.Type)
			results = append(results, rType)
		}
		if len(results) == 1 {
			sig.WriteString(" ")
			sig.WriteString(results[0])
		} else {
			sig.WriteString(" (")
			sig.WriteString(strings.Join(results, ", "))
			sig.WriteString(")")
		}
	}

	return sig.String()
}

func compactTypeDecl(spec *ast.TypeSpec) string {
	typeName := spec.Name.Name

	switch t := spec.Type.(type) {
	case *ast.StructType:
		if t.Fields == nil || len(t.Fields.List) == 0 {
			return fmt.Sprintf("type %s struct{}", typeName)
		}

		exported := []string{}
		totalFields := 0
		for _, field := range t.Fields.List {
			if len(field.Names) > 0 {
				for _, name := range field.Names {
					totalFields++
					if isExported(name.Name) {
						fType := compactType(field.Type)
						exported = append(exported, fmt.Sprintf("%s %s", name.Name, fType))
					}
				}
			} else {
				// Embedded field
				totalFields++
				fType := compactType(field.Type)
				exported = append(exported, fType)
			}
		}

		if len(exported) > 0 && len(exported) <= 3 {
			return fmt.Sprintf("type %s struct{%s}", typeName, strings.Join(exported, "; "))
		}
		return fmt.Sprintf("type %s struct{%d fields}", typeName, totalFields)

	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return fmt.Sprintf("type %s interface{}", typeName)
		}

		methods := []string{}
		for _, method := range t.Methods.List {
			if len(method.Names) > 0 {
				methods = append(methods, method.Names[0].Name)
			} else {
				methods = append(methods, compactType(method.Type))
			}
		}

		if len(methods) <= 5 {
			return fmt.Sprintf("type %s interface{%s}", typeName, strings.Join(methods, ", "))
		}
		return fmt.Sprintf("type %s interface{%d methods}", typeName, len(methods))

	default:
		typeStr := compactType(spec.Type)
		return fmt.Sprintf("type %s = %s", typeName, typeStr)
	}
}

func compactType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + compactType(t.X)
	case *ast.ArrayType:
		return "[]" + compactType(t.Elt)
	case *ast.MapType:
		return fmt.Sprintf("map[%s]%s", compactType(t.Key), compactType(t.Value))
	case *ast.SelectorExpr:
		pkg := compactType(t.X)
		if isStdlib(pkg) {
			return t.Sel.Name
		}
		return pkg + "." + t.Sel.Name
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct"
	case *ast.FuncType:
		return "func"
	case *ast.ChanType:
		return "chan"
	case *ast.Ellipsis:
		return "..." + compactType(t.Elt)
	default:
		return "?"
	}
}

func isExported(name string) bool {
	if name == "" {
		return false
	}
	return name[0] >= 'A' && name[0] <= 'Z'
}

func isStdlib(pkg string) bool {
	stdlibs := []string{
		"context", "fmt", "os", "io", "http", "time", "sync",
		"errors", "strings", "bytes", "encoding", "net", "path",
	}
	for _, s := range stdlibs {
		if pkg == s {
			return true
		}
	}
	return false
}

func isRelevantFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	relevant := []string{
		".md", ".txt", ".json", ".yaml", ".yml", ".toml",
		".sh", ".py", ".js", ".ts", ".sql", ".proto",
	}
	for _, r := range relevant {
		if ext == r {
			return true
		}
	}
	return false
}

func shouldIgnore(path string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		if matched {
			return true
		}
		if strings.Contains(path, pattern) {
			return true
		}
	}
	return false
}
