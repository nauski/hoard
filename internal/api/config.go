package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/nauski/hoard/internal/config"
)

// coldView is the editable offsite-storage config. The secret key and repo
// password are write-only: their values never leave the server, only whether
// they are set.
type coldView struct {
	Repository    string `json:"repository"`
	S3AccessKeyID string `json:"s3_access_key_id"`
	PasswordSet   bool   `json:"password_set"`
	SecretSet     bool   `json:"secret_set"`
}

type configResponse struct {
	Schedule       config.Schedule  `json:"schedule"`
	Retention      config.Retention `json:"retention"`
	Alert          config.Alert     `json:"alert"`
	HotRepo        string           `json:"hot_repo"`
	HotPasswordSet bool             `json:"hot_password_set"`
	Cold           coldView         `json:"cold"`
	RecoveryKitAck bool             `json:"recovery_kit_ack"`
}

func (s *Server) configView() configResponse {
	c := s.cfg.Load()
	return configResponse{
		Schedule:       c.Schedule,
		Retention:      c.Retention,
		Alert:          c.Alert,
		HotRepo:        c.Hot.Repository,
		HotPasswordSet: c.Hot.Password != "",
		Cold: coldView{
			Repository:    c.Cold.Repository,
			S3AccessKeyID: c.Cold.S3AccessKeyID,
			PasswordSet:   c.Cold.Password != "",
			SecretSet:     c.Cold.S3SecretAccessKey != "",
		},
		RecoveryKitAck: c.RecoveryKitAck,
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.configView())
}

// handleSetConfig updates the editable settings — schedule, retention, alert,
// and the offsite (cold) storage: any S3-compatible endpoint, its keys, and the
// repo password. Applies live (clients are rebuilt from config per-op) and
// persists to the config file. The write-only secret/password are only changed
// when a non-empty value is supplied; otherwise the current one is kept.
func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule  *config.Schedule  `json:"schedule"`
		Retention *config.Retention `json:"retention"`
		Alert     *config.Alert     `json:"alert"`
		Cold      *struct {
			Repository        *string `json:"repository"`
			S3AccessKeyID     *string `json:"s3_access_key_id"`
			S3SecretAccessKey *string `json:"s3_secret_access_key"`
			Password          *string `json:"password"`
		} `json:"cold"`
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
		if req.Cold != nil {
			if req.Cold.Repository != nil {
				c.Cold.Repository = *req.Cold.Repository
			}
			if req.Cold.S3AccessKeyID != nil {
				c.Cold.S3AccessKeyID = *req.Cold.S3AccessKeyID
			}
			if req.Cold.S3SecretAccessKey != nil && *req.Cold.S3SecretAccessKey != "" {
				c.Cold.S3SecretAccessKey = *req.Cold.S3SecretAccessKey
			}
			if req.Cold.Password != nil && *req.Cold.Password != "" {
				c.Cold.Password = *req.Cold.Password
			}
		}
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "saved in memory but failed to persist: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.configView())
}

// handleTestCold verifies the current cold repo is reachable (restic can open
// it), so the user can confirm new storage settings before relying on them.
func (s *Server) handleTestCold(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := s.cold().Snapshots(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleInitCold initializes the cold repo if it doesn't exist yet — restic
// creates the encrypted store in the (empty) bucket and seals it with the
// configured repo password. Safe to run against an already-initialized repo:
// EnsureInit is a no-op if the repo already opens. This is the GUI equivalent
// of `restic init`, so switching to fresh storage needs no shell.
func (s *Server) handleInitCold(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.cold().EnsureInit(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
