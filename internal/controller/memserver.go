package controller

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultDailyLimit = 100000

// MemServer is an in-memory reference implementation of the controller-v1
// protocol (docs/controller-protocol.md) suitable for Go unit tests.
type MemServer struct {
	HostToken   string
	ClientToken string
	Now         func() time.Time
	DailyLimit  int

	mu       sync.Mutex
	hosts    map[string]storedHost
	usageDay string
	requests int
}

type storedHost struct {
	record HostRecord
	seq    int64
}

func (s *MemServer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *MemServer) dailyLimit() int {
	if s.DailyLimit > 0 {
		return s.DailyLimit
	}
	return defaultDailyLimit
}

func (s *MemServer) countRequest(now time.Time) (day string, count int) {
	day = now.UTC().Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usageDay != day {
		s.usageDay = day
		s.requests = 0
	}
	s.requests++
	return day, s.requests
}

// ServeHTTP implements http.Handler for the controller-v1 endpoints.
func (s *MemServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	day, count := s.countRequest(now)

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
		writeJSON(w, http.StatusOK, HealthResponse{OK: true, Protocol: ProtocolVersion})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/hosts":
		if !s.authorize(w, r, s.ClientToken, s.HostToken) {
			return
		}
		s.handleHosts(w, now)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/usage":
		if !s.authorize(w, r, s.ClientToken, s.HostToken) {
			return
		}
		writeJSON(w, http.StatusOK, UsageResponse{Day: day, Requests: count, Limit: s.dailyLimit()})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/hosts/") && strings.HasSuffix(r.URL.Path, "/state"):
		if !s.authorize(w, r, s.HostToken, s.ClientToken) {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/hosts/"), "/state")
		if strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, "not_found", "Unknown route.")
			return
		}
		s.handlePutState(w, r, id, now)
	default:
		writeError(w, http.StatusNotFound, "not_found", "Unknown route.")
	}
}

func (s *MemServer) authorize(w http.ResponseWriter, r *http.Request, required, otherRole string) bool {
	tok, ok := parseBearer(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid bearer token.")
		return false
	}
	if constantEqual(tok, required) {
		return true
	}
	if constantEqual(tok, otherRole) {
		writeError(w, http.StatusForbidden, "forbidden", "Token lacks permission for this endpoint.")
		return false
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid bearer token.")
	return false
}

func parseBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	return tok, tok != ""
}

func constantEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *MemServer) handlePutState(w http.ResponseWriter, r *http.Request, id string, now time.Time) {
	if !ValidHostID(id) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid host ID.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("Request body exceeds the %d-byte limit.", MaxBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Unable to read request body.")
		return
	}

	var req PutStateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Malformed JSON body.")
		return
	}
	if req.ID != id || req.Name == "" || req.SentAt.IsZero() {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing or mismatched host state envelope fields.")
		return
	}

	s.mu.Lock()
	if s.hosts == nil {
		s.hosts = make(map[string]storedHost)
	}
	s.hosts[id] = storedHost{
		record: HostRecord{
			ID:     id,
			Name:   req.Name,
			SeenAt: now,
			State:  req.State,
		},
		seq: req.Seq,
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *MemServer) handleHosts(w http.ResponseWriter, now time.Time) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.hosts))
	for id := range s.hosts {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	hosts := make([]HostRecord, 0, len(ids))
	for _, id := range ids {
		rec := s.hosts[id].record
		rec.Stale = now.Sub(rec.SeenAt) > StaleAfter
		hosts = append(hosts, rec)
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, HostsResponse{Now: now, Hosts: hosts})
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
