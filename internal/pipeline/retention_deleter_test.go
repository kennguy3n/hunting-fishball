package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeVectorDeleter struct {
	err  error
	tenants []string
	ids  [][]string
}

func (f *fakeVectorDeleter) Delete(_ context.Context, tenantID string, ids []string) error {
	f.tenants = append(f.tenants, tenantID)
	f.ids = append(f.ids, ids)
	return f.err
}

type fakeMetaDeleter struct {
	err     error
	tenants []string
	docs    []string
}

func (f *fakeMetaDeleter) DeleteByDocument(_ context.Context, tenantID, documentID string) ([]string, error) {
	f.tenants = append(f.tenants, tenantID)
	f.docs = append(f.docs, documentID)
	return nil, f.err
}

func TestComboRetentionDeleter_RoutesBothTiers(t *testing.T) {
	t.Parallel()
	v := &fakeVectorDeleter{}
	m := &fakeMetaDeleter{}
	d := pipeline.NewComboRetentionDeleter(m, v)
	if err := d.DeleteChunk(context.Background(), "tenant-a", "doc-1", "chunk-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(v.ids) != 1 || v.ids[0][0] != "chunk-1" {
		t.Fatalf("vector delete not invoked: %+v", v.ids)
	}
	if len(m.docs) != 1 || m.docs[0] != "doc-1" {
		t.Fatalf("metadata delete not invoked: %+v", m.docs)
	}
}

func TestComboRetentionDeleter_NilTiersAreNoops(t *testing.T) {
	t.Parallel()
	d := pipeline.NewComboRetentionDeleter(nil, nil)
	if err := d.DeleteChunk(context.Background(), "tenant-a", "doc-1", "chunk-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestComboRetentionDeleter_VectorFailureIsReturnedButMetaStillRuns(t *testing.T) {
	t.Parallel()
	v := &fakeVectorDeleter{err: errors.New("qdrant unreachable")}
	m := &fakeMetaDeleter{}
	d := pipeline.NewComboRetentionDeleter(m, v)
	err := d.DeleteChunk(context.Background(), "tenant-a", "doc-1", "chunk-1")
	if err == nil {
		t.Fatalf("expected error to bubble")
	}
	if len(m.docs) != 1 {
		t.Fatalf("metadata delete should still execute when vector fails")
	}
}

func TestComboRetentionDeleter_RequiresTenantAndChunk(t *testing.T) {
	t.Parallel()
	d := pipeline.NewComboRetentionDeleter(&fakeMetaDeleter{}, &fakeVectorDeleter{})
	if err := d.DeleteChunk(context.Background(), "", "doc", "chunk"); err == nil {
		t.Fatalf("expected error for missing tenant")
	}
	if err := d.DeleteChunk(context.Background(), "tenant", "doc", ""); err == nil {
		t.Fatalf("expected error for missing chunk_id")
	}
}
