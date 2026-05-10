package project

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

func ContentFilter(signature string, fn FileContentFilterFunc) FileContentFilter {
	return FileContentFilter{
		Signature: strings.TrimSpace(signature),
		fn:        fn,
	}
}

func (f FileContentFilter) Apply(ctx context.Context, rt *Runtime, file FileContent) ([]byte, error) {
	if f.fn == nil {
		return append([]byte(nil), file.Content...), nil
	}
	return f.fn(ctx, rt, file)
}

func Filtered(path any, filter FileContentFilter) FilteredInput {
	value := ""
	switch typed := path.(type) {
	case string:
		value = typed
	case InputGlob:
		value = string(typed)
	case fmt.Stringer:
		value = typed.String()
	default:
		value = fmt.Sprint(path)
	}
	return FilteredInput{Path: strings.TrimSpace(value), Filter: filter}
}

func LinesStartingWith(prefixes ...string) FileContentFilter {
	prefixes = cleanPrefixes(prefixes)
	return ContentFilter("lines-starting-with:"+jsonString(prefixes), func(ctx context.Context, rt *Runtime, file FileContent) ([]byte, error) {
		_ = ctx
		_ = rt
		return filterLines(file.Content, func(line string) bool {
			line = strings.TrimSpace(line)
			for _, prefix := range prefixes {
				if strings.HasPrefix(line, prefix) {
					return true
				}
			}
			return false
		}), nil
	})
}

func GoCommentLinesStartingWith(prefixes ...string) FileContentFilter {
	prefixes = cleanPrefixes(prefixes)
	return ContentFilter("go-comment-lines-starting-with:"+jsonString(prefixes), func(ctx context.Context, rt *Runtime, file FileContent) ([]byte, error) {
		_ = ctx
		_ = rt
		return filterLines(file.Content, func(line string) bool {
			line = normalizeGoCommentLine(line)
			for _, prefix := range prefixes {
				if strings.HasPrefix(line, prefix) {
					return true
				}
			}
			return false
		}), nil
	})
}

func GoStructDeclarations() FileContentFilter {
	return ContentFilter("go-struct-declarations-with-docs:v1", func(ctx context.Context, rt *Runtime, file FileContent) ([]byte, error) {
		_ = ctx
		_ = rt
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file.Path, file.Content, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, ok := typeSpec.Type.(*ast.StructType); !ok {
					continue
				}
				if out.Len() > 0 {
					out.WriteByte('\n')
				}
				doc := typeSpec.Doc
				if doc == nil {
					doc = gen.Doc
				}
				writeCommentGroup(&out, doc)
				printSpec := *typeSpec
				printSpec.Doc = nil
				printSpec.Comment = nil
				if err := printer.Fprint(&out, fset, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{&printSpec}}); err != nil {
					return nil, err
				}
				out.WriteByte('\n')
			}
		}
		return out.Bytes(), nil
	})
}

func writeCommentGroup(out *bytes.Buffer, group *ast.CommentGroup) {
	if group == nil {
		return
	}
	for _, comment := range group.List {
		text := strings.TrimSpace(comment.Text)
		if text == "" {
			continue
		}
		out.WriteString(text)
		out.WriteByte('\n')
	}
}

func CombineContentFilters(filters ...FileContentFilter) FileContentFilter {
	clean := make([]FileContentFilter, 0, len(filters))
	signatures := make([]string, 0, len(filters))
	for _, filter := range filters {
		if strings.TrimSpace(filter.Signature) == "" && filter.fn == nil {
			continue
		}
		clean = append(clean, filter)
		signatures = append(signatures, filter.Signature)
	}
	return ContentFilter("combine:"+jsonString(signatures), func(ctx context.Context, rt *Runtime, file FileContent) ([]byte, error) {
		var out bytes.Buffer
		for _, filter := range clean {
			data, err := filter.Apply(ctx, rt, file)
			if err != nil {
				return nil, err
			}
			if len(bytes.TrimSpace(data)) == 0 {
				continue
			}
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString("filter:")
			out.WriteString(filter.Signature)
			out.WriteByte('\n')
			out.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				out.WriteByte('\n')
			}
		}
		return out.Bytes(), nil
	})
}

func filterLines(content []byte, keep func(string) bool) []byte {
	var out bytes.Buffer
	for _, line := range strings.Split(string(content), "\n") {
		normalized := strings.TrimSpace(line)
		if normalized == "" || !keep(line) {
			continue
		}
		out.WriteString(normalized)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func normalizeGoCommentLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "//")
	line = strings.TrimPrefix(line, "/*")
	line = strings.TrimPrefix(line, "*")
	line = strings.TrimSuffix(line, "*/")
	return strings.TrimSpace(line)
}

func cleanPrefixes(prefixes []string) []string {
	out := make([]string, 0, len(prefixes))
	seen := map[string]bool{}
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	return out
}

func jsonString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}
