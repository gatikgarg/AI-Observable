package openai

import (
	"net/url"
	"time"
)

// unix converts provider epoch seconds to UTC. Zero stays zero so the store can
// keep "not reported" distinct from 1970.
func unix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}
