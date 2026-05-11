// audit_export.go — Round-7 Task 14.
//
// GET /v1/admin/audit/export?format=csv|jsonl streams the audit
// trail in the requested format. Streaming avoids buffering the
// full result set in memory — relevant when an operator exports
// six months of audit data.
//
// Filters mirror the existing audit search endpoint: since,
// until, action, resource_type, resource_id, q (PayloadSearch).
// The tenant is always derived from the authenticated context;
// the endpoint refuses to read cross-tenant data.
package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// AuditExportReader is the narrow surface the export handler
// needs. The real audit Repository already satisfies this — the
// interface exists so tests can inject fakes without standing up
// a SQLite repo.
type AuditExportReader interface {
	List(ctx context.Context, f audit.ListFilter) (*audit.ListResult, error)
}

// AuditExportHandler is the HTTP surface.
type AuditExportHandler struct {
	reader AuditExportReader
	audit  AuditWriter
}

// NewAuditExportHandler validates inputs.
func NewAuditExportHandler(reader AuditExportReader, aw AuditWriter) (*AuditExportHandler, error) {
	if reader == nil {
		return nil, errors.New("audit_export: nil reader")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &AuditExportHandler{reader: reader, audit: aw}, nil
}

// Register mounts GET /v1/admin/audit/export.
func (h *AuditExportHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/audit/export", h.export)
}

func (h *AuditExportHandler) export(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	format := c.DefaultQuery("format", "jsonl")
	if format != "csv" && format != "jsonl" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format must be csv or jsonl"})
		return
	}
	var since, until time.Time
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		since = t
	}
	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		until = t
	}
	filter := audit.ListFilter{
		TenantID:      tenantID,
		ResourceType:  c.Query("resource_type"),
		ResourceID:    c.Query("resource_id"),
		PayloadSearch: c.Query("q"),
		Since:         since,
		Until:         until,
		PageSize:      200,
	}
	if a := c.Query("action"); a != "" {
		filter.Actions = []audit.Action{audit.Action(a)}
	}

	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	if format == "csv" {
		c.Writer.Header().Set("Content-Type", "text/csv")
		c.Writer.Header().Set("Content-Disposition", `attachment; filename="audit_export.csv"`)
		w := csv.NewWriter(c.Writer)
		_ = w.Write([]string{"id", "tenant_id", "actor_id", "action", "resource_type", "resource_id", "created_at", "trace_id"})
		if err := h.stream(c.Request.Context(), filter, func(rows []audit.AuditLog) error {
			for _, r := range rows {
				if err := w.Write([]string{
					r.ID, r.TenantID, r.ActorID, string(r.Action),
					r.ResourceType, r.ResourceID,
					r.CreatedAt.UTC().Format(time.RFC3339Nano),
					r.TraceID,
				}); err != nil {
					return err
				}
			}
			w.Flush()
			return w.Error()
		}); err != nil {
			_, _ = c.Writer.WriteString(fmt.Sprintf("\nERROR: %s\n", err.Error()))
		}
		w.Flush()
		return
	}
	c.Writer.Header().Set("Content-Type", "application/x-ndjson")
	c.Writer.Header().Set("Content-Disposition", `attachment; filename="audit_export.jsonl"`)
	enc := json.NewEncoder(c.Writer)
	count := 0
	if err := h.stream(c.Request.Context(), filter, func(rows []audit.AuditLog) error {
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				return err
			}
			count++
		}
		return nil
	}); err != nil {
		_, _ = c.Writer.WriteString(fmt.Sprintf("ERROR: %s\n", err.Error()))
	}
	actor := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actor, audit.ActionAuditExported, "audit", "",
		audit.JSONMap{"format": format, "rows": count}, "",
	))
}

func (h *AuditExportHandler) stream(ctx context.Context, filter audit.ListFilter, emit func([]audit.AuditLog) error) error {
	for safety := 0; safety < 1000; safety++ {
		res, err := h.reader.List(ctx, filter)
		if err != nil {
			return err
		}
		if res == nil || len(res.Items) == 0 {
			return nil
		}
		if err := emit(res.Items); err != nil {
			return err
		}
		if res.NextPageToken == "" {
			return nil
		}
		filter.PageToken = res.NextPageToken
	}
	return errors.New("audit_export: pagination safety limit exceeded")
}

// parseQueryInt is a small helper kept locally so the package
// stays free of strconv noise in the handler body.
func parseQueryInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
