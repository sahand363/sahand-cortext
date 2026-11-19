package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func (a *App) orchCreateJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Task                string                      `json:"task"`
		MaxRounds           int                         `json:"max_rounds"`
		MaxCalls            int                         `json:"max_calls"`
		AutoRetry           *bool                       `json:"auto_retry"`
		SwitchParticipant   *bool                       `json:"switch_participant"`
		AllowGlobalFallback *bool                       `json:"allow_global_fallback"`
		Participants        []orchestration.Participant `json:"participants"`
		Metadata            map[string]any              `json:"metadata"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req) != nil || strings.TrimSpace(req.Task) == "" {
		jsonOut(w, 400, map[string]string{"error": "Valid task description is required"})
		return
	}

	if req.MaxRounds > 20 {
		req.MaxRounds = 20
	}
	if req.MaxCalls > 50 {
		req.MaxCalls = 50
	}

	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	if req.AutoRetry != nil {
		req.Metadata["auto_retry"] = *req.AutoRetry
	}
	if req.SwitchParticipant != nil {
		req.Metadata["switch_participant"] = *req.SwitchParticipant
	}
	if req.AllowGlobalFallback != nil {
		req.Metadata["allow_global_fallback"] = *req.AllowGlobalFallback
	}

	if len(req.Participants) > 0 {
		for i := range req.Participants {
			p := strings.TrimSpace(req.Participants[i].Provider)
			m := strings.TrimSpace(req.Participants[i].Model)
			if strings.Contains(m, "::") {
				parts := strings.SplitN(m, "::", 2)
				p = parts[0]
				m = parts[1]
			}
			normProv := provider.CanonicalID(p)
			regProv, _, regErr := a.router.Registry().Get(normProv)
			if regErr != nil {
				jsonOut(w, 400, map[string]string{"error": regErr.Error()})
				return
			}
			m = regProv.NormalizeModelID(m)
			if m == "" {
				jsonOut(w, 400, map[string]string{"error": "Participant model ID cannot be empty"})
				return
			}
			if !a.isProviderConfigured(normProv) {
				jsonOut(w, 400, map[string]string{"error": "Provider " + regProv.Metadata().Name + " is not configured or disabled"})
				return
			}
			req.Participants[i].Provider = normProv
			req.Participants[i].Model = m
		}
		req.Metadata["participants"] = req.Participants
	}

	metaBytes, _ := json.Marshal(req.Metadata)
	job, err := a.orchStorage.CreateJob(r.Context(), req.Task, req.MaxRounds, req.MaxCalls, string(metaBytes))
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed creating job: " + err.Error()})
		return
	}

	if len(req.Participants) > 0 {
		_ = a.orchStorage.SaveParticipants(r.Context(), job.ID, req.Participants)
	}

	jsonOut(w, 201, job)
}

func (a *App) orchListJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}
	status := r.URL.Query().Get("status")

	jobs, err := a.orchStorage.ListJobs(r.Context(), limit, offset, status)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed listing jobs: " + err.Error()})
		return
	}
	jsonOut(w, 200, jobs)
}

func (a *App) orchGetJob(w http.ResponseWriter, r *http.Request) {
	rawPath := strings.TrimPrefix(r.URL.Path, "/api/orchestration/jobs/")
	if idx := strings.Index(rawPath, "/messages/"); idx != -1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		jID := rawPath[:idx]
		msgIDStr := rawPath[idx+len("/messages/"):]
		msgID, err := strconv.ParseInt(msgIDStr, 10, 64)
		if jID == "" || err != nil || msgID <= 0 {
			jsonOut(w, 400, map[string]string{"error": "Valid job ID and message ID are required"})
			return
		}
		msg, err := a.orchStorage.GetCollaborationMessageByID(r.Context(), jID, msgID)
		if err != nil {
			if errors.Is(err, orchestration.ErrMessageNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Collaboration message not found"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Failed loading collaboration message: " + err.Error()})
			return
		}
		jsonOut(w, 200, msg)
		return
	}

	jobID := rawPath
	jobID = strings.TrimSuffix(jobID, "/events")
	jobID = strings.TrimSuffix(jobID, "/messages")
	jobID = strings.TrimSuffix(jobID, "/participants")
	jobID = strings.TrimSuffix(jobID, "/details")
	jobID = strings.TrimSuffix(jobID, "/cancel")
	jobID = strings.TrimSuffix(jobID, "/pause")
	jobID = strings.TrimSuffix(jobID, "/retry")
	jobID = strings.TrimSuffix(jobID, "/iterate")
	jobID = strings.TrimSuffix(jobID, "/audit")
	jobID = strings.TrimSuffix(jobID, "/apply-audit")

	if jobID == "" {
		jsonOut(w, 400, map[string]string{"error": "Job ID is required"})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/events") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		events, err := a.orchStorage.GetJobEvents(r.Context(), jobID)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed retrieving events: " + err.Error()})
			return
		}
		jsonOut(w, 200, events)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/messages") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		msgs, err := a.orchStorage.GetCollaborationMessages(r.Context(), jobID)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed retrieving collaboration messages: " + err.Error()})
			return
		}
		jsonOut(w, 200, msgs)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/participants") {
		if r.Method == http.MethodGet {
			parts, err := a.orchStorage.GetParticipants(r.Context(), jobID)
			if err != nil {
				jsonOut(w, 500, map[string]string{"error": "Failed retrieving participants"})
				return
			}
			jsonOut(w, 200, parts)
			return
		} else if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var body struct {
				Participants []orchestration.Participant `json:"participants"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil || len(body.Participants) == 0 {
				jsonOut(w, 400, map[string]string{"error": "Invalid participants list"})
				return
			}
			for i := range body.Participants {
				p := strings.TrimSpace(body.Participants[i].Provider)
				m := strings.TrimSpace(body.Participants[i].Model)
				if strings.Contains(m, "::") {
					parts := strings.SplitN(m, "::", 2)
					p = parts[0]
					m = parts[1]
				}
				normProv := provider.CanonicalID(p)
				regProv, _, regErr := a.router.Registry().Get(normProv)
				if regErr != nil {
					jsonOut(w, 400, map[string]string{"error": regErr.Error()})
					return
				}
				m = regProv.NormalizeModelID(m)
				if m == "" || !a.isProviderConfigured(normProv) {
					jsonOut(w, 400, map[string]string{"error": "Invalid or unconfigured participant provider/model"})
					return
				}
				body.Participants[i].Provider = normProv
				body.Participants[i].Model = m
			}
			if err := a.orchStorage.SaveParticipants(r.Context(), jobID, body.Participants); err != nil {
				jsonOut(w, 500, map[string]string{"error": "Failed updating participants: " + err.Error()})
				return
			}
			_ = a.orchStorage.RecordEvent(r.Context(), jobID, "participants_updated", "running", "user", "Collaboration team roster updated mid-job", "{}")
			jsonOut(w, 200, map[string]bool{"ok": true})
			return
		}
		http.Error(w, "method not allowed", 405)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/cancel") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := a.orchStorage.CancelJob(r.Context(), jobID); err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found or already completed"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Cancellation failed: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": jobID, "status": "cancelled", "cancelled": true})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/pause") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := a.orchStorage.PauseJob(r.Context(), jobID); err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found or not running"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Pause failed: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": jobID, "status": "paused", "paused": true})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/retry") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := a.orchStorage.RetryJob(r.Context(), jobID); err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found or already running"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Retry failed: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": jobID, "status": "queued", "retried": true})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/iterate") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Instruction      string `json:"instruction"`
			ExtraRounds      int    `json:"extra_rounds"`
			ExtraCalls       int    `json:"extra_calls"`
			IncludeFullFiles *bool  `json:"include_full_files"`
			IncludeSummary   *bool  `json:"include_summary"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body) != nil || strings.TrimSpace(body.Instruction) == "" {
			jsonOut(w, 400, map[string]string{"error": "Please enter your revision instructions"})
			return
		}
		incFull := true
		if body.IncludeFullFiles != nil {
			incFull = *body.IncludeFullFiles
		}
		incSum := true
		if body.IncludeSummary != nil {
			incSum = *body.IncludeSummary
		}
		taggedInstruction := fmt.Sprintf("[REV_OPTS:full_files=%t,summary=%t]\n%s", incFull, incSum, strings.TrimSpace(body.Instruction))

		updatedJob, err := a.orchStorage.IterateJob(r.Context(), jobID, taggedInstruction, body.ExtraRounds, body.ExtraCalls)
		if err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found"})
				return
			}
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, updatedJob)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/audit") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid audit request payload"})
			return
		}
		prov := strings.TrimSpace(body.Provider)
		mod := strings.TrimSpace(body.Model)
		if strings.Contains(mod, "::") {
			parts := strings.SplitN(mod, "::", 2)
			prov = parts[0]
			mod = parts[1]
		}
		if prov == "" {
			prov = a.activeProvider()
		}
		if mod == "" {
			mod = a.activeModel()
		}
		auditMsg, err := a.orchEngine.AuditDeliverable(r.Context(), jobID, prov, mod)
		if err != nil {
			jsonOut(w, 502, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, auditMsg)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/apply-audit") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			MessageID int64 `json:"message_id"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body)
		updatedJob, err := a.orchStorage.ApplyAuditResult(r.Context(), jobID, body.MessageID)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, updatedJob)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/details") || r.URL.Query().Get("include") == "all" {
		details, err := a.orchStorage.GetJobDetails(r.Context(), jobID)
		if err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Error retrieving job details: " + err.Error()})
			return
		}
		jsonOut(w, 200, details)
		return
	}

	job, err := a.orchStorage.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, orchestration.ErrJobNotFound) {
			jsonOut(w, 404, map[string]string{"error": "Job not found"})
			return
		}
		jsonOut(w, 500, map[string]string{"error": "Error retrieving job: " + err.Error()})
		return
	}
	jsonOut(w, 200, job)
}
