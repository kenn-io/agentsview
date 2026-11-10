package imagetext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// passBudget limits when a pass may start another image.
var passBudget = 2 * time.Minute

const pageSize = 20

func (m *Manager) Run(ctx context.Context) error {
	if m.DB.ArchiveContent().OmitsToolContent() {
		return nil
	}
	deadline := time.Now().Add(passBudget)
	identity := m.Client.Identity()
	if m.Processor != "" {
		identity.Fingerprint = m.Processor
	}
	cutoff := time.Now()
	reader := &db.RetainedImageReader{}
	for {
		pending, err := m.DB.PendingImageText(ctx, identity.Fingerprint, cutoff, pageSize)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for _, hash := range pending {
			if !time.Now().Before(deadline) {
				return ctx.Err()
			}
			if err := m.processImage(ctx, identity, hash, reader); err != nil {
				return err
			}
		}
	}
}

// processImage reads the first retained copy whose bytes match hash. Only
// verified bytes can fail permanently; unusable copies leave a retry.
func (m *Manager) processImage(ctx context.Context, identity ocr.Identity, hash string, reader *db.RetainedImageReader) error {
	for offset := 0; ; offset++ {
		items, err := m.DB.ImageTextSources(ctx, hash, offset, 1)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			body, err := reader.Read(item.Content, item.CallID, item.EventID, item.Index, m.DB.AssetsDir(), glmocr.MaxDocumentBytes)
			if err != nil || len(body) == 0 {
				body, err = db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir(), glmocr.MaxDocumentBytes)
				if err != nil || len(body) == 0 {
					continue
				}
			}
			if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != hash {
				// A source can change without changing its length during a pass.
				body, err = db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir(), glmocr.MaxDocumentBytes)
				if err != nil {
					continue
				}
				if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != hash {
					continue
				}
			}
			// Process verifies the type against the bytes before sending a request.
			// Try image types so a stale label does not hide usable image data.
			for _, mediaType := range glmocr.SupportedMediaTypes() {
				if !strings.HasPrefix(mediaType, "image/") {
					continue
				}
				source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(body)), mediaType, int64(len(body)), hash)
				if err != nil {
					return err
				}
				result, err := m.Client.Process(ctx, source)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if ocr.ErrorKindOf(err) == ocr.ErrorInvalidInput {
						continue
					}
					return m.DB.SaveImageText(ctx, hash, identity, "", true)
				}
				var parts []string
				for _, unit := range result.Document.Units {
					parts = append(parts, unit.Text)
				}
				text := strings.TrimSpace(strings.Join(parts, "\n"))
				return m.DB.SaveImageText(ctx, hash, identity, text, false)
			}
			return m.DB.SaveImageTextTerminal(ctx, hash, identity)
		}
		if len(items) == 0 {
			break
		}
	}
	return m.DB.SaveImageText(ctx, hash, identity, "", true)
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
