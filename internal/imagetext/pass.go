package imagetext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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

// passBudget bounds when a pass starts requests; Docbank's request timeout bounds the one in flight.
var passBudget = 2 * time.Minute

const pageSize = 20

func (m *Manager) Run(ctx context.Context) error {
	if err := m.DB.BackfillImageTextOccurrences(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(passBudget)
	identity := m.Client.Identity()
	if m.Processor != "" {
		identity.Fingerprint = m.Processor
	}
	cutoff := time.Now()
	for {
		pending, err := m.DB.PendingImageText(ctx, identity.Fingerprint, cutoff, pageSize)
		if err != nil || len(pending) == 0 {
			return err
		}
		for _, hash := range pending {
			if !time.Now().Before(deadline) {
				return nil
			}
			if err := m.processImage(ctx, identity, hash); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) processImage(ctx context.Context, identity ocr.Identity, hash string) error {
	lastErr := errors.New("no retained source")
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
		if errors.Is(err, db.ErrImageTooLarge) {
			log.Printf("image text: skipping %s: %v", hash, err)
			return m.DB.SaveImageText(ctx, hash, identity, "", false)
		}
		sum := sha256.Sum256(body)
		if err != nil || hex.EncodeToString(sum[:]) != hash {
			if err == nil {
				err = errors.New("content hash mismatch")
			}
			lastErr = err
			continue
		}
		mediaType := http.DetectContentType(body)
		if !strings.HasPrefix(mediaType, "image/") {
			return m.DB.SaveImageText(ctx, hash, identity, "", false)
		}
		source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(body)), mediaType, int64(len(body)), hash)
		if err != nil {
			return err
		}
		result, err := m.Client.Process(ctx, source)
		if err != nil {
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
	log.Printf("image text: no readable copy of %s (%v); retrying with backoff", hash, lastErr)
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
