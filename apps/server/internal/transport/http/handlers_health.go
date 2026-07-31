package http

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// healthz reports that the process is alive.
//
// Deliberately checks nothing. Liveness answers "should this container be
// restarted", and restarting a healthy process because its database is briefly
// unreachable turns a database blip into a fleet-wide disconnect: every agent
// stream this replica holds would drop.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) error {
	return respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether this replica can serve requests.
//
// This one does check the database, because readiness answers "should traffic be
// sent here" — and a replica that cannot reach PostgreSQL should be taken out of
// the load balancer rather than serving 500s.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) error {
	// Short: a readiness probe that waits ten seconds for a hung database holds
	// the probe open past its own timeout, and the orchestrator reads that as a
	// failure anyway — just later and less clearly.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		// The cause is logged, not returned: a readiness endpoint is reachable
		// by anything that can route to the pod.
		s.log.WarnContext(ctx, "readiness check failed", "error", err)
		return respond(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "the database is unreachable",
		})
	}
	return respond(w, http.StatusOK, map[string]string{"status": "ready"})
}

// notFound renders unmatched routes as Problem Details.
//
// Without this, net/http answers with its own plain-text 404 and a client that
// parses application/problem+json everywhere gets a surprise exactly when
// something has already gone wrong.
func (s *Server) notFound(_ http.ResponseWriter, r *http.Request) error {
	return domain.NotFound("endpoint", r.Method+" "+r.URL.Path)
}

// uuidFrom parses a UUID from a request field.
func uuidFrom(raw, pointer string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.Validation(domain.FieldError{
			Pointer: pointer, Message: "must be a UUID",
		})
	}
	return id, nil
}
