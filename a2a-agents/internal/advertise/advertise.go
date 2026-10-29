// Package advertise adds Aquifer's pacing header to a resource's responses:
// X-Aqueduct-Max-Concurrent, the resource's total capacity. Aquifer (with
// shared destination limits) holds the total across its tenant queues from
// it; configurations A and B ignore it. B's dispatcher is given the same
// number in its configuration, so both know the capacity.
package advertise

import (
	"net/http"
	"strconv"
)

// Wrap sets the header on every response h writes, refusals included.
func Wrap(h http.Handler, maxConcurrent int) http.Handler {
	v := strconv.Itoa(maxConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Aqueduct-Max-Concurrent", v)
		h.ServeHTTP(w, r)
	})
}
