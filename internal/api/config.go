package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nauski/hoard/internal/hot"
	"github.com/nauski/hoard/internal/restic"

	"github.com/nauski/hoard/internal/config"
)

// offsiteView is one offsite storage as the dashboard sees it. The secret key
// and repo password are write-only: their values never leave the server, only
// whether they are set.
type offsiteView struct {
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Default            bool   `json:"default"`
	Repository         string `json:"repository"`
	S3AccessKeyID      string `json:"s3_access_key_id"`
	PasswordSet        bool   `json:"password_set"`
	SecretSet          bool   `json:"secret_set"`
	LimitUploadKiBps   int    `json:"limit_upload_kibps"`
	LimitDownloadKiBps int    `json:"limit_download_kibps"`
}

// smtpView is the editable email-alert config. The password is write-only:
// its value never leaves the server, only whether it is set.
type smtpView struct {
	Host        string `json:"host"`
	Port        string `json:"port"`
	Username    string `json:"username"`
	From        string `json:"from"`
	To          string `json:"to"`
	PasswordSet bool   `json:"password_set"`
}

type configResponse struct {
	Schedule       config.Schedule     `json:"schedule"`
	Retention      config.Retention    `json:"retention"`
	Alert          config.Alert        `json:"alert"`
	Anomaly        config.Anomaly      `json:"anomaly"`
	HotRepo        string              `json:"hot_repo"`
	HotPasswordSet bool                `json:"hot_password_set"`
	Offsite        []offsiteView       `json:"offsite"`
	Routing        map[string][]string `json:"routing"`
	SMTP           smtpView            `json:"smtp"`
	RecoveryKitAck bool                `json:"recovery_kit_ack"`
	// DashboardPasswordSet is write-only state: the password itself never
	// leaves the server. Clients lists the machines with their own repository.
	DashboardPasswordSet bool     `json:"dashboard_password_set"`
	PrivateRepos         bool     `json:"private_repos"`
	Clients              []string `json:"clients"`
}

func (s *Server) configView() configResponse {
	c := s.cfg.Load()
	return configResponse{
		Schedule:       c.Schedule,
		Retention:      c.Retention,
		Anomaly:        c.Anomaly,
		Alert:          c.Alert,
		HotRepo:        c.Hot.Repository,
		HotPasswordSet: c.Hot.Password != "",
		Offsite:        offsiteViews(c),
		Routing:        c.Routing,
		SMTP: smtpView{
			Host: c.SMTP.Host, Port: c.SMTP.Port, Username: c.SMTP.Username,
			From: c.SMTP.From, To: c.SMTP.To, PasswordSet: c.SMTP.Password != "",
		},
		RecoveryKitAck: c.RecoveryKitAck,

		DashboardPasswordSet: c.DashboardPassword != "",
		PrivateRepos:         c.PrivateRepos,
		Clients:              clientNames(c),
	}
}

func offsiteViews(c *config.Config) []offsiteView {
	out := make([]offsiteView, 0, len(c.Offsite))
	for _, o := range c.Offsite {
		out = append(out, offsiteView{
			ID: o.ID, Label: o.Label, Default: o.Default, Repository: o.Repository,
			S3AccessKeyID: o.S3AccessKeyID, PasswordSet: o.Password != "", SecretSet: o.S3SecretAccessKey != "",
			LimitUploadKiBps: o.LimitUploadKiBps, LimitDownloadKiBps: o.LimitDownloadKiBps,
		})
	}
	return out
}

// offsitePost is one storage in a POST /api/config body.
type offsitePost struct {
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Default            bool   `json:"default"`
	Repository         string `json:"repository"`
	Password           string `json:"password"`
	S3AccessKeyID      string `json:"s3_access_key_id"`
	S3SecretAccessKey  string `json:"s3_secret_access_key"`
	LimitUploadKiBps   int    `json:"limit_upload_kibps"`
	LimitDownloadKiBps int    `json:"limit_download_kibps"`
}

// applyOffsite replaces c.Offsite with the posted list: new entries get an id
// from their label, known ids keep their stored secrets when the posted value
// is empty, and ids that are gone are stripped from every routing list. On
// error c is left untouched.
func applyOffsite(c *config.Config, posted []offsitePost) error {
	old := map[string]config.Offsite{}
	for _, o := range c.Offsite {
		old[o.ID] = o
	}
	var list []config.Offsite
	var ids, labels []string
	for _, p := range posted {
		label := strings.TrimSpace(p.Label)
		switch {
		case label == "":
			return errors.New("every offsite storage needs a label")
		case slices.Contains(labels, strings.ToLower(label)):
			return fmt.Errorf("offsite label %q is used twice", label)
		case strings.TrimSpace(p.Repository) == "":
			return fmt.Errorf("offsite storage %q needs a repository", label)
		}
		labels = append(labels, strings.ToLower(label))
		prev, known := old[p.ID]
		id := p.ID
		if !known {
			id = config.NewOffsiteID(label, append(slices.Clone(ids), c.OffsiteIDs()...))
		}
		o := config.Offsite{ID: id, Label: label, Default: p.Default, Repo: config.Repo{
			Repository: p.Repository, S3AccessKeyID: p.S3AccessKeyID,
			Password: p.Password, S3SecretAccessKey: p.S3SecretAccessKey,
			LimitUploadKiBps: p.LimitUploadKiBps, LimitDownloadKiBps: p.LimitDownloadKiBps,
		}}
		if known && o.Password == "" {
			o.Password = prev.Password
		}
		if known && o.S3SecretAccessKey == "" {
			o.S3SecretAccessKey = prev.S3SecretAccessKey
		}
		ids = append(ids, id)
		list = append(list, o)
	}
	c.Offsite = list
	routing := make(map[string][]string, len(c.Routing))
	for h, r := range c.Routing {
		routing[h] = slices.DeleteFunc(slices.Clone(r), func(id string) bool { return !slices.Contains(ids, id) })
	}
	if len(routing) == 0 {
		routing = nil
	}
	c.Routing = routing
	return nil
}

func clientNames(c *config.Config) []string {
	names := make([]string, 0, len(c.Clients))
	for n := range c.Clients {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.configView())
}

// handleSetConfig updates the editable settings — schedule, retention, alert,
// and the offsite storage list (any restic repository, with its keys and repo
// password). Applies live (clients are rebuilt from config per-op) and
// persists to the config file. The write-only secret/password are only changed
// when a non-empty value is supplied; otherwise the current one is kept. A
// posted "cold" object or mirror.exclude_hosts is ignored: offsite and routing
// are the only source.
func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule  *config.Schedule  `json:"schedule"`
		Retention *config.Retention `json:"retention"`
		Anomaly   *config.Anomaly   `json:"anomaly"`
		// DashboardPassword sets a new dashboard password when non-empty.
		DashboardPassword string         `json:"dashboard_password"`
		Alert             *config.Alert  `json:"alert"`
		Offsite           *[]offsitePost `json:"offsite"`
		SMTP              *struct {
			Host, Port, Username, From, To, Password string
		} `json:"smtp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Retention != nil {
		if err := req.Retention.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Offsite != nil {
		probe := *s.cfg.Load()
		if err := applyOffsite(&probe, *req.Offsite); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	err := s.cfg.Update(func(c *config.Config) {
		if req.Anomaly != nil {
			c.Anomaly = *req.Anomaly
		}
		if req.DashboardPassword != "" {
			c.DashboardPassword = req.DashboardPassword
		}
		if req.Schedule != nil {
			c.Schedule = *req.Schedule
		}
		if req.Retention != nil {
			c.Retention = *req.Retention
		}
		if req.Alert != nil {
			c.Alert = *req.Alert
		}
		if req.Offsite != nil {
			_ = applyOffsite(c, *req.Offsite) // validated above
		}
		if req.SMTP != nil {
			c.SMTP.Host = req.SMTP.Host
			c.SMTP.Port = req.SMTP.Port
			c.SMTP.Username = req.SMTP.Username
			c.SMTP.From = req.SMTP.From
			c.SMTP.To = req.SMTP.To
			if req.SMTP.Password != "" {
				c.SMTP.Password = req.SMTP.Password
			}
		}
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "saved in memory but failed to persist: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.configView())
}

// handleTestOffsite verifies the storage named by ?id= is reachable (restic
// can open it), so the user can confirm settings before relying on them.
func (s *Server) handleTestOffsite(w http.ResponseWriter, r *http.Request) {
	cl, _, ok := s.offsite(r.URL.Query().Get("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown offsite storage"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := cl.Snapshots(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRetentionPreview reports which snapshots of each offsite storage the POSTed retention
// policy would forget — a read-only dry-run so the user can review before saving.
// When the policy has apply_to_hot set, it also reports what the hot repo would
// lose (hot_count / hot_snapshots).
func (s *Server) handleRetentionPreview(w http.ResponseWriter, r *http.Request) {
	var ret config.Retention
	if err := json.NewDecoder(r.Body).Decode(&ret); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := ret.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{}
	type storagePreview struct {
		ID        string            `json:"id"`
		Label     string            `json:"label"`
		Count     int               `json:"count"`
		Snapshots []restic.Snapshot `json:"snapshots"`
	}
	previews := []storagePreview{}
	for _, o := range s.cfg.Load().Offsite {
		rm, err := s.offsiteClient(o).ForgetDryRun(ctx, ret)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": o.Label + ": " + err.Error()})
			return
		}
		previews = append(previews, storagePreview{o.ID, o.Label, len(rm), rm})
	}
	resp["offsite"] = previews
	if ret.ApplyToHot {
		var rm []restic.Snapshot
		for _, k := range s.hot().Keys() {
			part, err := s.hot().Repo(k).ForgetDryRun(ctx, ret)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": hot.Label(k) + ": " + err.Error()})
				return
			}
			rm = append(rm, part...)
		}
		resp["hot_count"], resp["hot_snapshots"] = len(rm), rm
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleInitOffsite initializes the storage named by ?id= if it does not
// exist yet. The new repository copies the chunker parameters of the shared hot
// repository, so identical files deduplicate when several repositories are
// copied into it. Safe against an already-initialized repo (InitLike is a
// no-op then). The GUI equivalent of `restic init`.
func (s *Server) handleInitOffsite(w http.ResponseWriter, r *http.Request) {
	cl, _, ok := s.offsite(r.URL.Query().Get("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown offsite storage"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	hotShared := s.sched.Hot().RepoConfig(hot.Shared)
	if err := cl.InitLike(ctx, &hotShared); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
