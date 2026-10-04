package identity

import "testing"

func TestLoadOrCreateStable(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil || first != second || len(first) != 32 {
		t.Fatalf("node ID changed: %q, %q, %v", first, second, err)
	}
}
