package memoryv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestMemoryProtos_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []proto.Message{
		&WriteMemoryRequest{
			TenantId:  "t1",
			UserId:    "u1",
			SessionId: "s1",
			Content:   "remember this",
			Metadata:  map[string]string{"k": "v"},
		},
		&WriteMemoryResponse{Id: "m1", CreatedAt: 1},
		&SearchMemoryRequest{
			TenantId:  "t1",
			UserId:    "u1",
			SessionId: "s1",
			Query:     "what was that",
			TopK:      5,
			Scopes: []MemoryScope{
				MemoryScope_MEMORY_SCOPE_USER,
				MemoryScope_MEMORY_SCOPE_SESSION,
			},
		},
		&SearchMemoryResponse{Results: []*SearchMemoryResult{
			{Record: &MemoryRecord{Id: "m1", TenantId: "t1", Content: "x"}, Score: 0.9},
		}},
		&DeleteMemoryRequest{TenantId: "t1", Id: "m1"},
		&DeleteMemoryResponse{Deleted: true},
	}

	for _, want := range cases {
		wire, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal %T: %v", want, err)
		}
		got := proto.Clone(want)
		proto.Reset(got)
		if err := proto.Unmarshal(wire, got); err != nil {
			t.Fatalf("Unmarshal %T: %v", want, err)
		}
		if !proto.Equal(want, got) {
			t.Fatalf("%T round-trip mismatch", want)
		}
	}
}

func TestMemoryScope_StringNonEmpty(t *testing.T) {
	t.Parallel()
	if MemoryScope_MEMORY_SCOPE_USER.String() == "" {
		t.Fatal("enum String() empty")
	}
}
