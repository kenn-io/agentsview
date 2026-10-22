package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/assets"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
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
			var err error
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
		if err := walkMarkdownImages(content, check); err != nil {
			return err
		}
	}
	return rows.Err()
}

func walkMarkdownImages(content string, visit func(string) error) error {
	source := []byte(content)
	return ast.Walk(goldmark.New().Parser().Parse(text.NewReader(source)), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if image, ok := node.(*ast.Image); ok && entering {
			return ast.WalkContinue, visit(string(image.Destination))
		}
		return ast.WalkContinue, nil
	})
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
	_, image := assets.ExtForMediaType(mime.TypeByExtension(ext))
	return digest, ext, valid && digest == strings.ToLower(digest) && image
}

// MarkdownAssetImages excludes links and code examples by walking image nodes.
func MarkdownAssetImages(content string) []RetainedImage {
	var images []RetainedImage
	_ = walkMarkdownImages(content, func(ref string) error {
		if digest, ext, ok := assetImageDigest(ref); ok {
			mediaType := mime.TypeByExtension(ext)
			images = append(images, RetainedImage{Index: len(images), MediaType: mediaType, SHA256: digest})
		}
		return nil
	})
	return images
}
