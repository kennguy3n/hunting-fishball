package shard_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

type stubVersionRepo struct {
	last shard.ScopeFilter
	v    int64
	err  error
}

func (s *stubVersionRepo) LatestVersion(_ context.Context, f shard.ScopeFilter) (int64, error) {
	s.last = f
	return s.v, s.err
}

func TestVersionLookup_NilRepo_ReturnsZero(t *testing.T) {
	t.Parallel()
	l := shard.VersionLookup{}
	got, err := l.LatestShardVersion(context.Background(), "tenant-a", "channel-a", "hybrid")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestVersionLookup_DelegatesScope(t *testing.T) {
	t.Parallel()
	repo := &stubVersionRepo{v: 7}
	l := shard.VersionLookup{Repo: repo}
	got, err := l.LatestShardVersion(context.Background(), "tenant-a", "channel-a", "hybrid")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 7 {
		t.Fatalf("version=%d", got)
	}
	if repo.last.TenantID != "tenant-a" || repo.last.ChannelID != "channel-a" || repo.last.PrivacyMode != "hybrid" {
		t.Fatalf("scope=%+v", repo.last)
	}
}

func TestVersionLookup_PropagatesErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	repo := &stubVersionRepo{err: want}
	l := shard.VersionLookup{Repo: repo}
	_, err := l.LatestShardVersion(context.Background(), "tenant-a", "", "")
	if !errors.Is(err, want) {
		t.Fatalf("err=%v want=%v", err, want)
	}
}
