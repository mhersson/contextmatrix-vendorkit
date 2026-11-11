package claudecode

import (
	"slices"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// RateLimit is one rate_limit_event.
type RateLimit struct {
	Status   string                       // allowed | allowed_warning | rejected
	Window   string                       // rateLimitType: the binding window
	ResetsAt time.Time                    // resetsAt of the binding window, UTC; zero when absent
	Windows  []protocol.SubscriptionUsage // unifiedWindows, sorted by Window
}

// parseRateLimit tolerates every field missing: a CLI talking to an endpoint
// that sends no rate-limit headers reports only the status.
func parseRateLimit(info *rateLimitInfo) RateLimit {
	if info == nil {
		return RateLimit{}
	}

	rl := RateLimit{Status: info.Status, Window: info.RateLimitType, ResetsAt: unixUTC(info.ResetsAt)}

	for name, w := range info.UnifiedWindows {
		// A window with an unknown reset is dropped so a zero time never
		// reaches the wire.
		if w.ResetsAt == nil {
			continue
		}

		rl.Windows = append(rl.Windows, protocol.SubscriptionUsage{
			Window: name, Utilization: w.Utilization, Status: w.Status, ResetsAt: unixUTC(w.ResetsAt),
		})
	}

	slices.SortFunc(rl.Windows, func(a, b protocol.SubscriptionUsage) int { return strings.Compare(a.Window, b.Window) })

	return rl
}

func unixUTC(sec *float64) time.Time {
	if sec == nil {
		return time.Time{}
	}

	return time.Unix(int64(*sec), 0).UTC()
}
