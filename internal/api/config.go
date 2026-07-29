package api

import (
	"encoding/json"
	"net/http"

	"github.com/nauski/hoard/internal/config"
)

// configResponse is the editable settings plus read-only infra (repos) and
// booleans for whether secrets are configured (values never leave the server).
type configResponse struct {
	Schedule        config.Schedule  `json:"schedule"`
	Retention       config.Retention `json:"retention"`
	Alert           config.Alert     `json:"alert"`
	HotRepo         string           `json:"hot_repo"`
	ColdRepo        string           `json:"cold_repo"`
	HotPasswordSet  bool             `json:"hot_password_set"`
	ColdPasswordSet bool             `json:"cold_password_set"`
	S3KeysSet       bool             `json:"s3_keys_set"`
}

func (s *Server) configView() configResponse {
	c := s.cfg.Load()
	return configResponse{
		Schedule:        c.Schedule,
		Retention:       c.Retention,
		Alert:           c.Alert,
		HotRepo:         c.Hot.Repository,
		ColdRepo:        c.Cold.Repository,
		HotPasswordSet:  c.Hot.Password != "",
		ColdPasswordSet: c.Cold.Password != "",
		S3KeysSet:       c.Cold.S3AccessKeyID != "",
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.configView())
}

// handleSetConfig updates the editable settings (schedule / retention / alert),
// applies them live, and persists them to the config file. Repos and secrets
// are managed at deploy time and are not editable here.
func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule  *config.Schedule  `json:"schedule"`
		Retention *config.Retention `json:"retention"`
		Alert     *config.Alert     `json:"alert"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		if req.Schedule != nil {
			c.Schedule = *req.Schedule
		}
		if req.Retention != nil {
			c.Retention = *req.Retention
		}
		if req.Alert != nil {
			c.Alert = *req.Alert
		}
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "saved in memory but failed to persist: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.configView())
}
