package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Uploads (v2.2): images and files in chat.
//
// Limits, per user (from config.json — change them there, then restart):
//   - MaxUploadMB (default 50 MB) — largest single file
//   - MaxUploadsPerUserPerWeek (default 5) — files per user per week
//   - MaxUploadBytesPerUserPerDay (default 150 MB) — bytes per user per day
//   - MaxUploadBytesPerUserTotal (default 200 MB) — bytes a user may
//     have stored at once; freed when a message expires or is deleted
//
// Images (jpg, png, webp, gif — including animated GIFs) are shown big
// in the chat. Other allowed files show as a download card.
// Executables and scripts (.exe, .js, .html, …) are rejected.

var uploadImageExts = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
}

var uploadFileExts = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".zip":  "application/zip",
	".mp3":  "audio/mpeg",
	".mp4":  "video/mp4",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// uploadMimeFor returns the expected MIME type for an extension and
// whether it is an image. Unknown extensions are rejected.
func uploadMimeFor(ext string) (string, bool, bool) {
	ext = strings.ToLower(ext)
	if m, ok := uploadImageExts[ext]; ok {
		return m, true, true
	}
	if m, ok := uploadFileExts[ext]; ok {
		return m, false, true
	}
	return "", false, false
}

// validateUpload checks one file before it is stored. size is bytes.
func validateUpload(fileName string, size int64, maxMB int) (ext, mimeType string, isImage bool, err error) {
	ext = strings.ToLower(filepath.Ext(fileName))
	mimeType, isImage, ok := uploadMimeFor(ext)
	if !ok {
		return "", "", false, fmt.Errorf("that file type is not allowed (%s) — images (jpg, png, webp, gif), pdf, txt, zip, mp3, mp4 and Office documents are", ext)
	}
	maxBytes := int64(maxMB) * 1024 * 1024
	if size > maxBytes {
		return "", "", false, fmt.Errorf("file is too big — max %d MB per file", maxMB)
	}
	if size <= 0 {
		return "", "", false, fmt.Errorf("file is empty")
	}
	return ext, mimeType, isImage, nil
}

// checkUploadQuota enforces the per-user weekly, daily and total limits.
// Returns a Persian/English error message when the user is over a limit.
func (s *Server) checkUploadQuota(name string, size int64) error {
	count7d, bytes24h, totalBytes := s.store.AttachmentStats(name)
	if count7d >= s.cfg.MaxUploadsPerUserPerWeek {
		return fmt.Errorf("you have sent %d files this week — the limit is %d files per week", count7d, s.cfg.MaxUploadsPerUserPerWeek)
	}
	if bytes24h+size > s.cfg.MaxUploadBytesPerUserPerDay {
		return fmt.Errorf("that would pass your daily limit of %s per day", formatBytes(s.cfg.MaxUploadBytesPerUserPerDay))
	}
	if totalBytes+size > s.cfg.MaxUploadBytesPerUserTotal {
		return fmt.Errorf("you already have %s stored — the limit is %s per user (delete old messages or wait for them to expire)", formatBytes(totalBytes), formatBytes(s.cfg.MaxUploadBytesPerUserTotal))
	}
	return nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// safeFileName strips any path from an uploaded name and limits its length.
func safeFileName(name string) string {
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if len([]rune(name)) > 100 {
		ext := filepath.Ext(name)
		base := string([]rune(name)[:100-len([]rune(ext))])
		name = base + ext
	}
	return name
}

// handleRoomUpload is POST /api/rooms/{id}/upload — multipart form,
// field "file", optional field "text" (a caption).
// The file becomes a message in that room, with its attachment.
func (s *Server) handleRoomUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil {
		return
	}
	if !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusForbidden, "join this room first")
		return
	}
	maxBytes := int64(s.cfg.MaxUploadMB) * 1024 * 1024
	// Limit the whole request body to the file limit + a little slack,
	// so a huge upload is stopped early instead of filling the disk.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024*1024)
	if err := r.ParseMultipartForm(maxBytes + 1024*1024); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file is too big — max %d MB per file", s.cfg.MaxUploadMB))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no file uploaded — choose a file first")
		return
	}
	defer file.Close()
	ext, mimeType, isImage, err := validateUpload(header.Filename, header.Size, s.cfg.MaxUploadMB)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.checkUploadQuota(sess.DisplayName, header.Size); err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	fileID, err := newTokenHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create file")
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "uploads", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store file")
		return
	}
	path := filepath.Join(dir, fileID+ext)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store file")
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		writeErr(w, http.StatusInternalServerError, "could not save file")
		return
	}
	_ = out.Close()
	att := &Attachment{
		ID: fileID, FileName: safeFileName(header.Filename),
		Size: header.Size, Mime: mimeType, IsImage: isImage, Ext: ext,
		URL: "/api/uploads/" + id + "/" + fileID + ext,
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if len([]rune(text)) > 1000 {
		_ = os.Remove(path)
		writeErr(w, http.StatusBadRequest, "caption is too long (max 1000 characters)")
		return
	}
	m, err := s.store.AddAttachment(id, sess.DisplayName, text, room.TTLSeconds, att)
	if err != nil {
		_ = os.Remove(path)
		writeErr(w, http.StatusInternalServerError, "could not save message")
		return
	}
	s.broker.Publish(id, Event{Type: "message", Message: &m})
	writeJSON(w, http.StatusCreated, m)
}

// handleUploadDownload is GET /api/uploads/{roomID}/{fileID.ext}.
// Only a logged-in user who has joined that room (or the OP) may
// download it — hidden rooms' files stay hidden the same way.
func (s *Server) handleUploadDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/uploads/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	roomID, fileName := parts[0], parts[1]
	if strings.Contains(roomID, ".") || strings.Contains(roomID, "/") || strings.Contains(fileName, "/") {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	room := s.rooms.Get(roomID)
	if room == nil || !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	// Find the message that owns this file, so we know its real
	// name, MIME type, and whether it has expired.
	var att *Attachment
	var modTime time.Time
	for _, m := range s.store.Last(roomID, 1000) {
		if m.Attachment != nil && m.Attachment.ID+m.Attachment.Ext == fileName {
			att = m.Attachment
			modTime = m.CreatedAt
			break
		}
	}
	if att == nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	path := filepath.Join(s.cfg.DataDir, "uploads", roomID, att.ID+att.Ext)
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", att.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if att.IsImage {
		// Images are shown in the chat, not downloaded.
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Disposition", `attachment; filename="`+mime.QEncoding.Encode("utf-8", att.FileName)+`"`)
	}
	http.ServeContent(w, r, att.FileName, modTime, f)
}
