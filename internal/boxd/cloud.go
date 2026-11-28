package boxd

import (
	"net/http"
	"slices"
	"strings"

	"github.com/a-Fig/agent-box/internal/dash"
)

// Cloud hosts on the page (#280; dash/cloud.go says how the reports flow). boxd only keeps what box
// reported, in memory: a restart forgets the reports and turns the pause off, and the next probe
// reports again.

func (s *Server) cloudRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/cloud/usage", control(http.HandlerFunc(s.cloudUsage)))
	mux.Handle("POST /api/cloud/pause", control(http.HandlerFunc(s.cloudPause)))
}

func (s *Server) cloudUsage(w http.ResponseWriter, r *http.Request) {
	var b dash.CloudUsageBody
	if !decode(w, r, &b) {
		return
	}
	if err := b.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, err.Error())
		return
	}
	if !s.W.ReportCloud(b, s.device(r)) {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "boxd keeps at most "+itoa(dash.MaxCloudHosts)+" cloud hosts")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cloudPause(w http.ResponseWriter, r *http.Request) {
	var b dash.CloudPauseBody
	if !decode(w, r, &b) {
		return
	}
	writeJSON(w, http.StatusOK, s.W.PauseCloud(b.Paused, s.device(r)))
}

// ReportCloud keeps a cloud usage report and announces it. It is false when the report names a
// host past dash.MaxCloudHosts.
func (w *Watcher) ReportCloud(b dash.CloudUsageBody, by string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if h := b.Host; h != nil {
		if w.cloud == nil {
			w.cloud = map[string]dash.CloudHost{}
		}
		if _, ok := w.cloud[h.Name]; !ok && len(w.cloud) >= dash.MaxCloudHosts {
			return false
		}
		c := *h
		if c.Budget != nil {
			g := *c.Budget
			c.Budget = &g
		}
		c.ReportedAt, c.ReportedBy = w.Now().UTC(), by
		w.cloud[c.Name] = c
	}
	if b.Controller != nil {
		u := *b.Controller
		w.controller = &u
	}
	w.emit(dash.Event{Name: dash.EventCloud, Data: w.cloudState()})
	return true
}

// PauseCloud sets the page's cloud pause, announcing and recording a change.
func (w *Watcher) PauseCloud(paused bool, by string) dash.CloudState {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cloudPaused != paused {
		w.cloudPaused = paused
		action := "cloud resume"
		if paused {
			action = "cloud pause"
		}
		w.record(record{Type: recControl, At: w.Now().UTC(), Control: &controlRec{Action: action, Target: "cloud", By: by}})
		w.emit(dash.Event{Name: dash.EventCloud, Data: w.cloudState()})
	}
	return w.cloudState()
}

// cloudState copies the cloud fields, hosts by name; w.mu is held.
func (w *Watcher) cloudState() dash.CloudState {
	st := dash.CloudState{Cloud: []dash.CloudHost{}, CloudPaused: w.cloudPaused}
	for _, c := range w.cloud {
		if c.Budget != nil {
			g := *c.Budget
			c.Budget = &g
		}
		st.Cloud = append(st.Cloud, c)
	}
	slices.SortFunc(st.Cloud, func(a, b dash.CloudHost) int { return strings.Compare(a.Name, b.Name) })
	if w.controller != nil {
		u := *w.controller
		st.Controller = &u
	}
	return st
}
