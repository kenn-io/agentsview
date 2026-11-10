package imagetext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// passBudget bounds one scheduled pass; the poller resumes the rest on its
// next interval.
var passBudget = 2 * time.Minute

const (
	pageSize        = 20
	passSaveReserve = time.Second
)

func (m *Manager) Run(ctx context.Context) error {
	passCtx, cancel := context.WithTimeout(ctx, passBudget)
	defer cancel()
	workCtx, stopWork := context.WithTimeout(passCtx, passBudget-passSaveReserve)
	defer stopWork()
	identity := m.Client.Identity()
	if m.Processor != "" {
		identity.Fingerprint = m.Processor
	}
	cutoff := time.Now()
	for {
		pending, err := m.DB.PendingImageText(passCtx, identity.Fingerprint, cutoff, pageSize)
		if err != nil {
			if passCtx.Err() != nil && ctx.Err() == nil {
				return nil
			}
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for _, hash := range pending {
			if workCtx.Err() != nil {
				return ctx.Err()
			}
			if err := m.processImage(workCtx, identity, hash); err != nil {
				if workCtx.Err() != nil && passCtx.Err() == nil {
					return m.DB.SaveImageText(passCtx, hash, identity, "", true)
				}
				if passCtx.Err() != nil && ctx.Err() == nil {
					return nil
				}
				return err
			}
		}
	}
}

// processImage reads the first retained copy whose bytes match hash. Only
// verified bytes can fail permanently; unusable copies leave a retry.
func (m *Manager) processImage(ctx context.Context, identity ocr.Identity, hash string) error {
	for offset := 0; ; offset += pageSize {
		items, err := m.DB.ImageTextSources(ctx, hash, offset, pageSize)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			body, err := db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir(), glmocr.MaxDocumentBytes)
			if err != nil || len(body) == 0 {
				continue
			}
			if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != hash {
				continue
			}
			mediaType := detectMediaType(body)
			if !slices.Contains(glmocr.SupportedMediaTypes(), mediaType) || !strings.HasPrefix(mediaType, "image/") {
				return m.DB.SaveImageTextTerminal(ctx, hash, identity)
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
					return m.DB.SaveImageTextTerminal(ctx, hash, identity)
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
		if len(items) < pageSize {
			break
		}
	}
	return m.DB.SaveImageText(ctx, hash, identity, "", true)
}

// detectMediaType follows Docbank's sniffing so a label never decides the
// type sent for OCR.
func detectMediaType(body []byte) string {
	if len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP" {
		return "image/webp"
	}
	return http.DetectContentType(body)
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
