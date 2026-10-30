package pricing

import "testing"

func TestCurrentDeepSeekV4ModelsArePriced(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4-flash", "deepseek-v4-pro"} {
		if _, ok := Lookup(model); !ok {
			t.Errorf("%s has no price", model)
		}
	}
	f, ok := Lookup("deepseek-flash")
	if !ok {
		t.Fatal("deepseek-flash has no price")
	}
	if f.Context != 1_000_000 {
		t.Fatalf("deepseek-flash context=%d, want 1000000", f.Context)
	}
}
