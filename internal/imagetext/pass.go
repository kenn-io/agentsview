package imagetext

import (
	"bytes"
	"context"
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
		for _, hash := range pending {
			if err := m.processImage(ctx, identity, hash); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) processImage(ctx context.Context, identity ocr.Identity, hash string) error {
	retry := false
	for offset := 0; ; offset += 20 {
		items, err := m.DB.ImageTextSources(ctx, hash, offset, 20)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
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
			body, offloaded, err := db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir())
			if err != nil {
				retry = true
				continue
			}
			source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(body)), image.MediaType, *image.Size, item.Hash)
			if err != nil {
				continue
			}
			imageCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			result, err := m.Client.Process(imageCtx, source)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if ocr.ErrorKindOf(err) != ocr.ErrorInvalidInput {
					return m.DB.SaveImageText(ctx, hash, identity, "", true)
				}
				if offloaded {
					retry = true
				}
				continue
			}
			var parts []string
			for _, unit := range result.Document.Units {
				parts = append(parts, unit.Text)
			}
			text := strings.TrimSpace(strings.Join(parts, "\n"))
			return m.DB.SaveImageText(ctx, item.Hash, identity, text, false)
		}
		if len(items) < 20 {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if retry {
		return m.DB.SaveImageText(ctx, hash, identity, "", true)
	}
	return m.DB.SaveImageTextTerminal(ctx, hash, identity)
}

// ProcessorKey includes the output policy while excluding the service address.
func ProcessorKey(policy glmocr.Policy) (string, error) {
	values := policy.Values()
	canonical, err := glmocr.NewPolicy(glmocr.PolicyConfig{
		Endpoint: glmocr.DefaultEndpoint, Model: values.Model, ServedModel: values.ServedModel,
		ModelRevision: values.ModelRevision, SDKRevision: values.SDKRevision,
		LayoutModel: values.LayoutModel, LayoutRevision: values.LayoutRevision,
		MaxDocumentBytes: values.MaxDocumentBytes, MaxResponseBytes: values.MaxResponseBytes,
		MaxUnits: values.MaxUnits, NormalizePolicy: policy.NormalizePolicy(),
	})
	if err != nil {
		return "", fmt.Errorf("build OCR policy: %w", err)
	}
	return canonical.Fingerprint(), nil
}
