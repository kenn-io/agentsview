package imagetext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// passBudget bounds extraction, including a request in flight.
var passBudget = 2 * time.Minute

const pageSize = 20

func (m *Manager) Run(ctx context.Context) error {
	if m.DB.ArchiveContent().OmitsToolContent() {
		return nil
	}
	if err := m.DB.BackfillImageTextOccurrences(ctx); err != nil {
		return err
	}
	passCtx, cancel := context.WithTimeout(ctx, passBudget)
	defer cancel()
	identity := m.Client.Identity()
	if m.Processor != "" {
		identity.Fingerprint = m.Processor
	}
	cutoff := time.Now()
	for {
		pending, err := m.DB.PendingImageText(passCtx, identity.Fingerprint, cutoff, pageSize)
		if err != nil {
			return passError(ctx, passCtx, err)
		}
		if len(pending) == 0 {
			return nil
		}
		for _, hash := range pending {
			if err := m.processImage(passCtx, identity, hash); err != nil {
				return passError(ctx, passCtx, err)
			}
		}
	}
}

func passError(parent, pass context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(pass.Err(), context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (m *Manager) processImage(ctx context.Context, identity ocr.Identity, hash string) error {
	for offset := 0; ; offset++ {
		items, err := m.DB.ImageTextSources(ctx, hash, offset, 1)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		item := items[0]
		body, err := db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir(), glmocr.MaxDocumentBytes)
		sum := sha256.Sum256(body)
		if err != nil || len(body) == 0 || hex.EncodeToString(sum[:]) != hash {
			continue
		}
		mediaType := http.DetectContentType(body)
		if !strings.HasPrefix(mediaType, "image/") || !slices.Contains(glmocr.SupportedMediaTypes(), mediaType) {
			return m.DB.SaveImageText(ctx, hash, identity, "", false)
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
				return m.DB.SaveImageText(ctx, hash, identity, "", false)
			}
			if saveErr := m.DB.SaveImageText(ctx, hash, identity, "", true); saveErr != nil {
				return saveErr
			}
			return err
		}
		var parts []string
		for _, unit := range result.Document.Units {
			parts = append(parts, unit.Text)
		}
		return m.DB.SaveImageText(ctx, hash, identity, strings.TrimSpace(strings.Join(parts, "\n")), false)
	}
	if err := m.DB.SaveImageText(ctx, hash, identity, "", true); err != nil {
		return err
	}
	return fmt.Errorf("no readable retained copy for image %s", hash)
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
