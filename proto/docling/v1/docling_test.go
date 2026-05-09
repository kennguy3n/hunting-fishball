package doclingv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestParseDocumentRequest_RoundTrip(t *testing.T) {
	t.Parallel()

	want := &ParseDocumentRequest{
		TenantId:    "tenant-a",
		DocumentId:  "doc-1",
		Content:     []byte{0x01, 0x02, 0x03},
		ContentType: "application/pdf",
		Metadata:    map[string]string{"path": "/foo/bar.pdf"},
	}

	wire, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got := &ParseDocumentRequest{}
	if err := proto.Unmarshal(wire, got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Fatalf("round-trip mismatch:\n want=%+v\n got =%+v", want, got)
	}
}

func TestParseDocumentResponse_RoundTrip(t *testing.T) {
	t.Parallel()

	want := &ParseDocumentResponse{
		Blocks: []*ParsedBlock{
			{
				BlockId:      "b1",
				Text:         "Hello",
				Type:         BlockType_BLOCK_TYPE_HEADING,
				HeadingLevel: 1,
				PageNumber:   1,
				Position:     0,
			},
			{
				BlockId: "b2",
				Text:    "world",
				Type:    BlockType_BLOCK_TYPE_PARAGRAPH,
			},
		},
		Structure: &DocumentStructure{
			Title:     "doc",
			Author:    "ken",
			Language:  "en",
			PageCount: 4,
			Metadata:  map[string]string{"source": "drive"},
		},
	}

	wire, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got := &ParseDocumentResponse{}
	if err := proto.Unmarshal(wire, got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Fatalf("round-trip mismatch:\n want=%+v\n got =%+v", want, got)
	}
}

func TestBlockTypeEnum_StringNonEmpty(t *testing.T) {
	t.Parallel()

	if BlockType_BLOCK_TYPE_PARAGRAPH.String() == "" {
		t.Fatalf("enum String() returned empty")
	}
}
