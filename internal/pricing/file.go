package pricing

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

//go:embed prices.json
var files embed.FS

type priceFile struct {
	Version string              `json:"version"`
	Models  map[string]fileRate `json:"models"`
}

type fileRate struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Context    int64   `json:"context"`
}

func init() {
	b, err := files.ReadFile("prices.json")
	if err != nil {
		status.LastError = err.Error()
		return
	}
	if err := loadInto(b, embedded); err != nil {
		status.LastError = err.Error()
		return
	}
	var f priceFile
	_ = json.Unmarshal(b, &f)
	status.Version = f.Version
	status.Models = len(f.Models)
}

func loadInto(b []byte, dst map[string]Price) error {
	var f priceFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	for name, r := range f.Models {
		dst[name] = Price{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, Context: r.Context}
	}
	return nil
}

// LoadFile loads the optional user price file. A missing file is not an error:
// it simply leaves the embedded table in charge. A malformed file is reported
// through FileStatus and leaves the previous table untouched.
func LoadFile(path string) error {
	mu.Lock()
	defer mu.Unlock()
	status.Path = path
	status.ModTime = time.Time{}
	if path == "" {
		overrides = map[string]Price{}
		status.LastError = ""
		return nil
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		overrides = map[string]Price{}
		status.LastError = ""
		return nil
	}
	if err != nil {
		status.LastError = err.Error()
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		status.LastError = err.Error()
		return err
	}
	next := map[string]Price{}
	if err = loadInto(b, next); err != nil {
		status.LastError = err.Error()
		return fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	overrides = next
	status.ModTime = st.ModTime().UTC()
	status.LastError = ""
	return nil
}

// FileStatus reports the active user price file.
func FileStatus() Status {
	mu.RLock()
	defer mu.RUnlock()
	s := status
	s.Models = modelCountLocked()
	return s
}

// SetOverrides is used by tests to inject a table without touching disk.
func SetOverrides(p map[string]Price) {
	mu.Lock()
	defer mu.Unlock()
	overrides = p
}
