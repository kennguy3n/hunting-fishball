// integrity.go — Round-13 Task 12.
//
// Audit log append-only verification. The endpoint walks the
// most recent N audit_logs rows for the caller's tenant and
// computes a running SHA-256 hash chain. Tampering with any
// historical row, or deleting one, changes the head hash, so the
// chain provides a tamper-evident snapshot that operators can
// periodically poll and store outside Postgres.
//
// The chain is intentionally derived (not materialized) — we do
// not store the hashes on disk, because doing so would defeat
// the integrity guarantee. Operators are expected to record the
// returned head hash + entry count in a separate, append-only
// system (e.g. an S3 bucket with object-lock).
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/gin-gonic/gin"
)

// IntegrityResponse is the JSON envelope.
type IntegrityResponse struct {
	TenantID   string `json:"tenant_id"`
	EntryCount int    `json:"entry_count"`
	HeadHash   string `json:"head_hash"`
	FirstEntry string `json:"first_entry_id,omitempty"`
	LastEntry  string `json:"last_entry_id,omitempty"`
	Algorithm  string `json:"algorithm"`
}

// IntegrityHandler serves GET /v1/admin/audit/integrity.
type IntegrityHandler struct {
	repo *Repository
}

// NewIntegrityHandler constructs the handler.
func NewIntegrityHandler(repo *Repository) (*IntegrityHandler, error) {
	if repo == nil {
		return nil, errors.New("audit.NewIntegrityHandler: repo required")
	}
	return &IntegrityHandler{repo: repo}, nil
}

// Register mounts the endpoint on rg.
func (h *IntegrityHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/audit/integrity", h.get)
}

func (h *IntegrityHandler) get(c *gin.Context) {
	tenantID, _ := c.Get(TenantContextKey)
	tid, _ := tenantID.(string)
	if tid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	limit := 1000
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= maxPageSize {
			limit = v
		}
	}
	res, err := h.repo.List(c.Request.Context(), ListFilter{TenantID: tid, PageSize: limit})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	resp := ComputeIntegrity(tid, res.Items)
	c.JSON(http.StatusOK, resp)
}

// ComputeIntegrity returns the chain head over logs. logs is
// expected newest-first (the order Repository.List returns); the
// function sorts oldest-first internally so adding a new row
// only changes the head hash, not the historical prefix.
func ComputeIntegrity(tenantID string, logs []AuditLog) IntegrityResponse {
	resp := IntegrityResponse{TenantID: tenantID, Algorithm: "sha256"}
	if len(logs) == 0 {
		// Empty chain: deterministic head hash of the tenant ID so
		// operators can still pin a value.
		sum := sha256.Sum256([]byte("audit-chain-empty:" + tenantID))
		resp.HeadHash = hex.EncodeToString(sum[:])
		return resp
	}
	// Sort oldest first so the chain has stable order.
	ordered := make([]AuditLog, len(logs))
	copy(ordered, logs)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].ID < ordered[j].ID
	})
	var prev [32]byte
	for _, l := range ordered {
		next := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s",
			hex.EncodeToString(prev[:]),
			l.ID,
			l.TenantID,
			l.ActorID,
			string(l.Action),
			l.ResourceType,
			l.ResourceID,
		)))
		prev = next
	}
	resp.EntryCount = len(ordered)
	resp.HeadHash = hex.EncodeToString(prev[:])
	resp.FirstEntry = ordered[0].ID
	resp.LastEntry = ordered[len(ordered)-1].ID
	return resp
}
