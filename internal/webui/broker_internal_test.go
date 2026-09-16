package webui

import (
	"reflect"
	"testing"
)

func TestBrokerResetReleasesBackingArray(t *testing.T) {
	b := newBrokerWithCap(8, 16)
	t.Cleanup(b.closeAll)

	for i := 0; i < 16; i++ {
		b.Log("info", "fill")
	}
	oldPtr := reflect.ValueOf(b.buffer).Pointer()
	if len(b.buffer) != 16 || cap(b.buffer) != 16 {
		t.Fatalf("pre-reset buffer len/cap = %d/%d, want 16/16", len(b.buffer), cap(b.buffer))
	}

	b.Reset()

	if len(b.buffer) != 0 {
		t.Fatalf("post-reset len = %d, want 0", len(b.buffer))
	}
	if cap(b.buffer) != b.capacity {
		t.Fatalf("post-reset cap = %d, want %d", cap(b.buffer), b.capacity)
	}
	newPtr := reflect.ValueOf(b.buffer).Pointer()
	if newPtr == oldPtr {
		t.Fatal("Reset reused the old backing array; want a fresh allocation so old Event.Data references are released")
	}
}
