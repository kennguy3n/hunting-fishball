package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	doclingv1 "github.com/kennguy3n/hunting-fishball/proto/docling/v1"
)

// ParseConfig configures Stage 2 (Parse).
type ParseConfig struct {
	// Client is the gRPC client for the Docling parsing service. Tests
	// inject a buf-conn-backed client; production wires a real
	// grpc.ClientConn.
	Client doclingv1.DoclingServiceClient

	// Timeout caps each ParseDocument call. Defaults to 30s.
	Timeout time.Duration

	// MaxAttempts is the number of retries for transient gRPC errors.
	// Defaults to 3.
	MaxAttempts int

	// InitialBackoff is the first retry sleep. Defaults to 200ms.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential retry backoff. Defaults to 5s.
	MaxBackoff time.Duration
}

func (c *ParseConfig) defaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.InitialBackoff == 0 {
		c.InitialBackoff = 200 * time.Millisecond
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 5 * time.Second
	}
}

// Parser is the Stage 2 worker.
type Parser struct {
	cfg ParseConfig
}

// NewParser constructs a Parser. Returns an error if Client is nil.
func NewParser(cfg ParseConfig) (*Parser, error) {
	if cfg.Client == nil {
		return nil, errors.New("parse: nil Client")
	}
	cfg.defaults()

	return &Parser{cfg: cfg}, nil
}

// Parse runs the gRPC ParseDocument call with retry / timeout. The
// returned []Block carries the document's privacy_label so downstream
// stages persist provenance correctly.
func (p *Parser) Parse(ctx context.Context, doc *Document) ([]Block, error) {
	if doc == nil {
		return nil, errors.New("parse: nil doc")
	}

	req := &doclingv1.ParseDocumentRequest{
		TenantId:    doc.TenantID,
		DocumentId:  doc.DocumentID,
		Content:     doc.Content,
		ContentType: doc.MIMEType,
		Metadata:    doc.Metadata,
	}

	var lastErr error
	backoff := p.cfg.InitialBackoff

	for attempt := 1; attempt <= p.cfg.MaxAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
		resp, err := p.cfg.Client.ParseDocument(callCtx, req)
		cancel()
		if err == nil {
			return p.adapt(doc, resp), nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, fmt.Errorf("parse: %w", err)
		}
		if attempt == p.cfg.MaxAttempts {
			break
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()

			return nil, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > p.cfg.MaxBackoff {
			backoff = p.cfg.MaxBackoff
		}
	}

	return nil, fmt.Errorf("parse: exhausted retries: %w", lastErr)
}

// adapt converts ParseDocumentResponse into the in-memory Block form
// the rest of the pipeline consumes. Privacy label is propagated from
// the document.
func (p *Parser) adapt(doc *Document, resp *doclingv1.ParseDocumentResponse) []Block {
	if resp == nil {
		return nil
	}

	out := make([]Block, 0, len(resp.GetBlocks()))
	for _, b := range resp.GetBlocks() {
		out = append(out, Block{
			BlockID:      b.GetBlockId(),
			Text:         b.GetText(),
			Type:         blockTypeName(b.GetType()),
			HeadingLevel: b.GetHeadingLevel(),
			Page:         b.GetPageNumber(),
			Position:     b.GetPosition(),
			PrivacyLabel: doc.PrivacyLabel,
		})
	}

	return out
}

// isRetryable returns true for gRPC error codes the spec treats as
// transient: Unavailable, DeadlineExceeded, ResourceExhausted, Aborted.
func isRetryable(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return true // non-gRPC errors are network-level → retry
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	default:
		return false
	}
}

func blockTypeName(t doclingv1.BlockType) string {
	switch t {
	case doclingv1.BlockType_BLOCK_TYPE_PARAGRAPH:
		return "paragraph"
	case doclingv1.BlockType_BLOCK_TYPE_HEADING:
		return "heading"
	case doclingv1.BlockType_BLOCK_TYPE_LIST_ITEM:
		return "list_item"
	case doclingv1.BlockType_BLOCK_TYPE_TABLE:
		return "table"
	case doclingv1.BlockType_BLOCK_TYPE_FIGURE:
		return "figure"
	case doclingv1.BlockType_BLOCK_TYPE_CAPTION:
		return "caption"
	case doclingv1.BlockType_BLOCK_TYPE_CODE:
		return "code"
	case doclingv1.BlockType_BLOCK_TYPE_FOOTNOTE:
		return "footnote"
	default:
		return "unknown"
	}
}
