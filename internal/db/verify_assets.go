package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/assets"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
)

// VerifyAssets checks content-addressed image references in retained messages and tool results.
// The caller holds the offline writer lock so the references cannot change.
func (d *DB) VerifyAssets(ctx context.Context, directory string) error {
	rows, err := d.getReader().QueryContext(ctx, `
		SELECT content, 0 FROM messages WHERE instr(content, 'asset://') > 0
		UNION ALL
		SELECT result_content, 1 FROM tool_calls WHERE instr(result_content, 'asset://') > 0
		UNION ALL
		SELECT content, 1 FROM tool_result_events WHERE instr(content, 'asset://') > 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var root *os.Root
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	seen := make(map[string]bool)
	check := func(ref string) error {
		if !strings.HasPrefix(ref, "asset://") || seen[ref] {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		want, _, valid := assetReferenceDigest(ref)
		if !valid {
			return nil
		}
		name := strings.TrimPrefix(ref, "asset://")
		if root == nil {
			root, err = os.OpenRoot(directory)
			if err != nil {
				return fmt.Errorf("opening required assets: %w", err)
			}
		}
		if !assets.IsCompleteObject(root, name, -1, want) {
			return fmt.Errorf("required asset %s is missing, corrupt, or has an unsupported file type", name)
		}
		seen[ref] = true
		return nil
	}
	markdown := parser.New()
	for rows.Next() {
		var content string
		var toolResult bool
		if err := rows.Scan(&content, &toolResult); err != nil {
			return err
		}
		if toolResult {
			var checkErr error
			scanSummarySections(content, func(_, _ int, raw jsontext.Value) {
				var blocks []jsontext.Value
				if json.Unmarshal(raw, &blocks) != nil {
					return
				}
				for _, block := range blocks {
					if checkErr != nil {
						return
					}
					placeholder, ok := parseOffloadedImagePlaceholder(block)
					if ok {
						checkErr = check(placeholder.ImageRef)
					}
				}
			})
			if checkErr != nil {
				return checkErr
			}
			continue
		}
		// Parse actual image nodes; examples inside code are not dependencies.
		if err := walkMarkdownImages(markdown, content, check); err != nil {
			return err
		}
	}
	return rows.Err()
}

func walkMarkdownImages(markdown parser.Parser, content string, visit func(string) error) error {
	return ast.Walk(parseAssetMarkdown(markdown, content), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if image, ok := node.(*ast.Image); ok && entering {
			return ast.WalkContinue, visit(image.Destination.Value([]byte(content)))
		}
		return ast.WalkContinue, nil
	})
}

// parseAssetMarkdown skips parsing content with no asset reference. A nil
// parser is built only when the content needs one.
func parseAssetMarkdown(markdown parser.Parser, content string) ast.Node {
	if !strings.Contains(content, "asset://") {
		return ast.NewDocument()
	}
	if markdown == nil {
		markdown = parser.New()
	}
	return markdown.Parse([]byte(content))
}

func assetReferenceDigest(ref string) (string, string, bool) {
	if !strings.HasPrefix(ref, "asset://") {
		return "", "", false
	}
	name := strings.TrimPrefix(ref, "asset://")
	ext := filepath.Ext(name)
	digest := strings.TrimSuffix(name, ext)
	decoded, err := hex.DecodeString(digest)
	return digest, ext, err == nil && len(decoded) == sha256.Size && filepath.Base(name) == name
}

func assetImageDigest(ref string) (string, string, bool) {
	digest, ext, valid := assetReferenceDigest(ref)
	_, image := assets.MediaTypeForExt(ext)
	return digest, ext, valid && digest == strings.ToLower(digest) && image
}

// MarkdownAssetImages excludes links and code examples by walking image nodes.
func MarkdownAssetImages(content string) []RetainedImage {
	var images []RetainedImage
	_ = walkMarkdownImages(nil, content, func(ref string) error {
		if digest, ext, ok := assetImageDigest(ref); ok {
			mediaType, _ := assets.MediaTypeForExt(ext)
			images = append(images, RetainedImage{Index: len(images), MediaType: mediaType, SHA256: digest})
		}
		return nil
	})
	return images
}
