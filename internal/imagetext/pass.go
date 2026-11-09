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
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/docbank/document/glmocr"
	"go.kenn.io/docbank/document/ocr"
)

type Manager struct {
	DB        *db.DB
	Client    *glmocr.Client
	Processor string
	mu        sync.Mutex
}

func (m *Manager) TryPass(ctx context.Context, _ extract.PassOptions) (bool, extract.PassResult, error) {
	if !m.mu.TryLock() {
		return false, extract.PassResult{}, nil
	}
	defer m.mu.Unlock()
	identity := m.Client.Identity()
	if m.Processor != "" {
		identity.Fingerprint = m.Processor
	}
	for {
		more, err := m.DB.BackfillImageTextOccurrences(ctx, 20)
		if err != nil {
			return true, extract.PassResult{}, err
		}
		if !more {
			break
		}
		if err := ctx.Err(); err != nil {
			return true, extract.PassResult{}, err
		}
	}
	for {
		pending, err := m.DB.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
		if err != nil {
			return true, extract.PassResult{}, err
		}
		if len(pending) == 0 {
			break
		}
		for _, item := range pending {
			if err := ctx.Err(); err != nil {
				return true, extract.PassResult{}, err
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
				if err := m.DB.SaveImageText(ctx, item.Hash, identity, "", true); err != nil {
					return true, extract.PassResult{}, err
				}
				continue
			}
			if image.Size == nil || *image.Size > glmocr.MaxDocumentBytes || (image.MediaType != "image/png" && image.MediaType != "image/jpeg" && image.MediaType != "image/webp") {
				if err := m.DB.SaveImageTextTerminal(ctx, item.Hash, identity); err != nil {
					return true, extract.PassResult{}, err
				}
				continue
			}
			body, err := db.ReadRetainedImage(item.Content, item.Index, m.DB.AssetsDir())
			var text string
			if err == nil {
				var source ocr.Source
				source, err = ocr.NewSource(io.NopCloser(bytes.NewReader(body)), image.MediaType, int64(len(body)), item.Hash)
				if err == nil {
					imageCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					result, processErr := m.Client.Process(imageCtx, source)
					cancel()
					err = processErr
					if err == nil {
						var parts []string
						for _, unit := range result.Document.Units {
							parts = append(parts, unit.Text)
						}
						text = strings.TrimSpace(strings.Join(parts, "\n"))
					}
				}
			}
			if ctx.Err() != nil {
				return true, extract.PassResult{}, ctx.Err()
			}
			if err := m.DB.SaveImageText(ctx, item.Hash, identity, text, err != nil || text == ""); err != nil {
				return true, extract.PassResult{}, err
			}
		}
	}
	return true, extract.PassResult{}, nil
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
