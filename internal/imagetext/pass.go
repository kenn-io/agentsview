package imagetext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/document/glmocr"
	"go.kenn.io/docbank/document/ocr"
)

type Manager struct {
	DB        *db.DB
	Client    *glmocr.Client
	Processor string
}

func (m *Manager) Run(ctx context.Context) error {
	identity := m.Client.Identity()
	if m.Processor != "" {
		identity.Fingerprint = m.Processor
	}
	for {
		more, err := m.DB.BackfillImageTextOccurrences(ctx, 20)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for {
		pending, err := m.DB.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for i := 0; i < len(pending); {
			j := i + 1
			for j < len(pending) && pending[j].Hash == pending[i].Hash {
				j++
			}
			if err := m.processImage(ctx, identity, pending[i:j]); err != nil {
				return err
			}
			i = j
		}
	}
}

func (m *Manager) processImage(ctx context.Context, identity ocr.Identity, items []db.PendingImageText) error {
	retry := false
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		var image *db.RetainedImage
		images := db.RetainedToolResultImages(item.Content)
		for i := range images {
			if images[i].Index == item.Index && images[i].SHA256 == item.Hash {
				image = &images[i]
				break
			}
		}
		if image == nil {
			retry = true
			continue
		}
		if image.Size == nil || *image.Size <= 0 || *image.Size > glmocr.MaxDocumentBytes || (image.MediaType != "image/png" && image.MediaType != "image/jpeg" && image.MediaType != "image/webp") {
			continue
		}
		body, err := db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir())
		if err != nil {
			retry = true
			continue
		}
		source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(body)), image.MediaType, int64(len(body)), item.Hash)
		if err != nil {
			continue
		}
		imageCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		result, err := m.Client.Process(imageCtx, source)
		cancel()
		if err != nil {
			if ocr.ErrorKindOf(err) != ocr.ErrorInvalidInput {
				retry = true
			}
			continue
		}
		var parts []string
		for _, unit := range result.Document.Units {
			parts = append(parts, unit.Text)
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" {
			retry = true
			continue
		}
		return m.DB.SaveImageText(ctx, item.Hash, identity, text, false)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if retry {
		return m.DB.SaveImageText(ctx, items[0].Hash, identity, "", true)
	}
	return m.DB.SaveImageTextTerminal(ctx, items[0].Hash, identity)
}

// ProcessorKey includes the output policy while excluding the service address.
func ProcessorKey(policy glmocr.Policy) (string, error) {
	values := policy.Values()
	values.Endpoint = ""
	data, err := json.Marshal(struct {
		Identity string
		Policy   glmocr.PolicyValues
	}{policy.Identity().Fingerprint, values})
	if err != nil {
		return "", fmt.Errorf("encode OCR policy: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
