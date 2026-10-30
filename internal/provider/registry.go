package provider

import (
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Adapter{}
)

// Register makes an adapter available by kind. Adapters call it from init();
// internal/provider/all performs the blank imports.
func Register(a Adapter) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[a.Kind()] = a
}

// For returns the adapter for kind. Unknown kinds produce an error that names
// every valid kind so the UI can show an actionable message.
func For(kind string) (Adapter, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if a, ok := registry[kind]; ok {
		return a, nil
	}
	valid := make([]string, 0, len(registry))
	for k := range registry {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	return nil, &unknownKindError{kind: kind, valid: valid}
}

type unknownKindError struct {
	kind  string
	valid []string
}

func (e *unknownKindError) Error() string {
	out := "provider: unknown kind " + quote(e.kind) + "; valid kinds: "
	for i, k := range e.valid {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

func quote(s string) string { return "\"" + s + "\"" }

// Kinds lists the registered kinds in sorted order.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CapabilitiesFor reports what kind can do without a network call. Unknown
// kinds report all-false rather than a hopeful guess.
func CapabilitiesFor(kind string) Capabilities {
	if a, err := For(kind); err == nil {
		return a.Capabilities()
	}
	return Capabilities{}
}
