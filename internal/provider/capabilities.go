package provider

// Capabilities is what a provider family can actually report. It drives the
// UI chips and whether a collector job is scheduled; it is never guessed.
type Capabilities struct {
	Models                bool `json:"models"`
	Usage                 bool `json:"usage"`
	Costs                 bool `json:"costs"`
	Balance               bool `json:"balance"`
	NativeUsageInResponse bool `json:"native_usage_in_response"`
}

// Labels returns the capability chips with a stable order.
func (c Capabilities) Labels() []string {
	var out []string
	if c.Models {
		out = append(out, "models")
	}
	if c.Usage {
		out = append(out, "usage")
	}
	if c.Costs {
		out = append(out, "costs")
	}
	if c.Balance {
		out = append(out, "balance")
	}
	if c.NativeUsageInResponse {
		out = append(out, "usage in response")
	}
	return out
}

// Has reports whether the named lane is available.
func (c Capabilities) Has(lane string) bool {
	switch lane {
	case "models":
		return c.Models
	case "usage":
		return c.Usage
	case "costs":
		return c.Costs
	case "balance":
		return c.Balance
	}
	return false
}
