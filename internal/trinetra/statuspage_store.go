package trinetra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

const (
	statusIncidentRetention = 365 * 24 * time.Hour
	statusHistoryDays       = 90
)

type serviceRuntimeState struct {
	State        core.ServiceState            `json:"state"`
	PendingState core.ServiceState            `json:"pending_state,omitempty"`
	PendingSince int64                        `json:"pending_since,omitempty"`
	History      map[string]core.ServiceState `json:"history,omitempty"`
}

type statusPageData struct {
	Services  []core.StatusService
	Incidents []core.StatusIncident
	State     map[string]*serviceRuntimeState

	// saved holds the bytes last written (or known on disk) per file, so
	// save() skips fsync'd writes when nothing changed.
	saved map[string][]byte
}

type servicesFileV1 struct {
	Version  int                  `json:"version"`
	Services []core.StatusService `json:"services"`
}

type incidentsFileV1 struct {
	Version   int                   `json:"version"`
	Incidents []core.StatusIncident `json:"incidents"`
}

type stateFileV1 struct {
	Version  int                             `json:"version"`
	Services map[string]*serviceRuntimeState `json:"services"`
}

func statusPageDir(stateDir string) string { return filepath.Join(stateDir, "status") }

// loadStatusJSON decodes path into v.
func loadStatusJSON(path string, v any, logf func(string, ...any)) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		if err = json.Unmarshal(b, v); err == nil {
			return
		}
	}
	aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	renErr := os.Rename(path, aside)
	if renErr != nil {
		logf("status page: %s unreadable (%v); could not move to %s (%v); starting empty", path, err, aside, renErr)
	} else {
		logf("status page: %s unreadable (%v); moved to %s and starting empty", path, err, aside)
	}
}

func loadStatusPage(dir string, logf func(string, ...any)) *statusPageData {
	var sf servicesFileV1
	var inf incidentsFileV1
	var stf stateFileV1
	loadStatusJSON(filepath.Join(dir, "services.json"), &sf, logf)
	loadStatusJSON(filepath.Join(dir, "incidents.json"), &inf, logf)
	loadStatusJSON(filepath.Join(dir, "state.json"), &stf, logf)
	d := &statusPageData{Services: sf.Services, Incidents: inf.Incidents, State: stf.Services}
	if d.State == nil {
		d.State = map[string]*serviceRuntimeState{}
	}
	// Drop nil entries that may have been decoded from null in JSON.
	for k, v := range d.State {
		if v == nil {
			delete(d.State, k)
		}
	}
	return d
}

func (d *statusPageData) writeIfChanged(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if prev, ok := d.saved[path]; ok && bytes.Equal(prev, b) {
		return nil
	}
	if err := writeFileAtomicSynced(path, b, 0o600); err != nil {
		return err
	}
	if d.saved == nil {
		d.saved = map[string][]byte{}
	}
	d.saved[path] = b
	return nil
}

func (d *statusPageData) save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := d.writeIfChanged(filepath.Join(dir, "services.json"), servicesFileV1{Version: 1, Services: d.Services}); err != nil {
		return err
	}
	if err := d.writeIfChanged(filepath.Join(dir, "incidents.json"), incidentsFileV1{Version: 1, Incidents: d.Incidents}); err != nil {
		return err
	}
	return d.writeIfChanged(filepath.Join(dir, "state.json"), stateFileV1{Version: 1, Services: d.State})
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (d *statusPageData) prune(now time.Time) {
	cut := now.Add(-statusIncidentRetention).Unix()
	kept := d.Incidents[:0]
	for _, inc := range d.Incidents {
		if inc.Status == core.IncidentResolved && inc.Resolved > 0 && inc.Resolved < cut {
			continue
		}
		kept = append(kept, inc)
	}
	d.Incidents = kept
	oldest := dayKey(now.AddDate(0, 0, -(statusHistoryDays - 1)))
	for _, st := range d.State {
		if st == nil {
			continue
		}
		for day := range st.History {
			if day < oldest {
				delete(st.History, day)
			}
		}
	}
}
