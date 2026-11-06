package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const sourcePath = "internal/server/server_test.go"

var proxyTests = map[string]bool{
	"TestInferenceForwardsSafeRequestHeaders":            true,
	"TestInferenceLogsUpstreamErrorResponse":             true,
	"TestInferenceStreamsUpstreamEvents":                 true,
	"TestPassthroughPreservesRequestAndResponse":         true,
	"TestPassthroughRemovesConnectionListedHeaders":      true,
	"TestPassthroughRemovesProxyConnectionHeader":        true,
	"TestSafeHTTPErrorOmitsURL":                          true,
	"TestResponseContinuesAfterRequestTimeout":           true,
	"TestForwardJSONRejectsExpiredPreparation":           true,
	"TestForwardJSONLogsUpstreamRequest":                 true,
	"TestResponseContextPreservesCallerDeadline":         true,
	"TestSlowClientResponseContinuesAfterRequestTimeout": true,
	"TestUpstreamErrorBodyTimeoutReturnsRequestTimeout":  true,
	"TestUpstreamTimeoutReturnsRequestTimeout":           true,
	"TestUpstreamUnreachableReturnsModelUpstreamError":   true,
}

type outputFile struct {
	name  string
	decls []ast.Decl
}

func main() {
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		panic(err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, sourcePath, source, parser.ParseComments)
	if err != nil {
		panic(err)
	}

	outputs := map[string]*outputFile{
		"server":    {name: sourcePath},
		"document":  {name: "internal/server/document_test.go"},
		"inference": {name: "internal/server/inference_test.go"},
		"proxy":     {name: "internal/server/proxy_test.go"},
		"helpers":   {name: "internal/server/test_helpers_test.go"},
	}

	for _, decl := range parsed.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		destination, err := classify(decl)
		if err != nil {
			panic(err)
		}
		outputs[destination].decls = append(outputs[destination].decls, decl)
	}

	for _, key := range []string{"server", "document", "inference", "proxy", "helpers"} {
		output := outputs[key]
		content := buildFile(source, fset, parsed.Imports, output.decls)
		if err := os.WriteFile(output.name, content, 0o644); err != nil {
			panic(err)
		}
		fmt.Printf("wrote %s\n", output.name)
	}
}

func classify(decl ast.Decl) (string, error) {
	switch declaration := decl.(type) {
	case *ast.FuncDecl:
		if receiverName(declaration) == "deadlineIgnoringConverter" {
			return "inference", nil
		}
		if receiverName(declaration) == "blockingHeaderWriter" {
			return "server", nil
		}
		name := declaration.Name.Name
		switch {
		case name == "testDependencies":
			return "helpers", nil
		case name == "writeTestPNG", name == "TestRequestDirectoryUsesOwnerOnlyPermissions", strings.HasPrefix(name, "TestDocumentParts"):
			return "document", nil
		case proxyTests[name], strings.HasPrefix(name, "TestPassthrough"), strings.HasPrefix(name, "TestForwardJSON"):
			return "proxy", nil
		case strings.HasPrefix(name, "TestResponses"),
			strings.HasPrefix(name, "TestInline"),
			name == "TestUnlimitedRequestBodyKeepsPerFileLimit",
			strings.HasPrefix(name, "TestInference"),
			strings.HasPrefix(name, "TestFileReferenceIDs"),
			strings.HasPrefix(name, "TestExpandChat"),
			strings.HasPrefix(name, "TestChat"):
			return "inference", nil
		case strings.HasPrefix(name, "Test"):
			return "server", nil
		default:
			return "", fmt.Errorf("unclassified function %s", name)
		}
	case *ast.GenDecl:
		for _, spec := range declaration.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				return "", fmt.Errorf("unclassified declaration at %d", declaration.Pos())
			}
			switch typeSpec.Name.Name {
			case "deadlineIgnoringConverter":
				return "inference", nil
			case "blockingHeaderWriter":
				return "server", nil
			default:
				return "", fmt.Errorf("unclassified type %s", typeSpec.Name.Name)
			}
		}
	}
	return "", fmt.Errorf("unclassified declaration at %d", decl.Pos())
}

func receiverName(declaration *ast.FuncDecl) string {
	if declaration.Recv == nil || len(declaration.Recv.List) == 0 {
		return ""
	}
	switch receiver := declaration.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		if name, ok := receiver.X.(*ast.Ident); ok {
			return name.Name
		}
	}
	return ""
}

func buildFile(source []byte, fset *token.FileSet, imports []*ast.ImportSpec, decls []ast.Decl) []byte {
	usedPackages := make(map[string]bool)
	for _, decl := range decls {
		ast.Inspect(decl, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if qualifier, ok := selector.X.(*ast.Ident); ok {
				usedPackages[qualifier.Name] = true
			}
			return true
		})
	}

	var builder strings.Builder
	builder.WriteString("package server\n\nimport (\n")
	wroteInternalImport := false
	for _, spec := range imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			panic(err)
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if !usedPackages[name] {
			continue
		}
		isInternalImport := strings.Contains(strings.Split(path, "/")[0], ".")
		if isInternalImport && !wroteInternalImport {
			builder.WriteByte('\n')
			wroteInternalImport = true
		}
		start := fset.Position(spec.Pos()).Offset
		end := fset.Position(spec.End()).Offset
		builder.WriteByte('\t')
		builder.Write(source[start:end])
		builder.WriteByte('\n')
	}
	builder.WriteString(")\n")
	for _, decl := range decls {
		start := fset.Position(decl.Pos()).Offset
		end := fset.Position(decl.End()).Offset
		builder.WriteString("\n")
		builder.Write(source[start:end])
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}
