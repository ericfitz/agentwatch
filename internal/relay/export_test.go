package relay

import "time"

// SetGrace shortens the offline grace window so a test can watch it expire.
// Call it before serving.
func SetGrace(rl *Relay, d time.Duration) { rl.grace = d }
