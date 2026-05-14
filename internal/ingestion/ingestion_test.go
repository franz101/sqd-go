package ingestion

import "testing"

func TestNextRequestRangeCursorOmitsToBlockWithLocalEnd(t *testing.T) {
	end := uint64(20)

	toBlock, label, ok := nextRequestRange(10, 0, &end, true)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock != nil {
		t.Fatalf("cursor request toBlock = %d, want nil", *toBlock)
	}
	if label != "[10-tail]" {
		t.Fatalf("label = %q, want [10-tail]", label)
	}
}

func TestNextRequestRangeBoundedUsesPageSizeAndEnd(t *testing.T) {
	end := uint64(20)

	toBlock, label, ok := nextRequestRange(10, 250, &end, false)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock == nil || *toBlock != 20 {
		t.Fatalf("bounded request toBlock = %v, want 20", toBlock)
	}
	if label != "[10-20]" {
		t.Fatalf("label = %q, want [10-20]", label)
	}
}
