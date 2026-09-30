package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// AddBuild records a build file, or returns the existing row when a file with
// the same sha256 is already known — the first upload stays its "uploaded by".
// A previously deleted build comes back to life with the new file path and
// the new uploader, since someone evidently needs it again.
func (s *Store) AddBuild(b model.Build) (model.Build, bool, error) {
	if old, err := s.buildBySHA(b.SHA256); err == nil {
		if old.DeletedAt == nil {
			return old, false, nil
		}
		_, err := s.db.Exec(`UPDATE builds SET deleted_at = NULL, path = ?, filename = ?,
			uploaded_by = ?, uploaded_at = ?, via = ?, source_url = ? WHERE id = ?`,
			b.Path, b.Filename, b.UploadedBy, b.UploadedAt, b.Via, b.SourceURL, old.ID)
		if err != nil {
			return model.Build{}, false, err
		}
		nb, err := s.Build(old.ID)
		return nb, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return model.Build{}, false, err
	}
	res, err := s.db.Exec(`INSERT INTO builds
		(sha256, filename, platform, format, size, package, app_name, version, build_code, min_os,
		 path, uploaded_by, uploaded_at, via, source_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.SHA256, b.Filename, string(b.Platform), b.Format, b.Size, b.Package, b.AppName,
		b.Version, b.BuildCode, b.MinOS, b.Path, b.UploadedBy, b.UploadedAt, b.Via, b.SourceURL)
	if err != nil {
		return model.Build{}, false, err
	}
	id, _ := res.LastInsertId()
	nb, err := s.Build(id)
	return nb, true, err
}

// FillBuildMeta sets package / version on a build that lacked them (an .aab's
// manifest is only readable once bundletool has expanded it for a device).
func (s *Store) FillBuildMeta(id int64, pkg, version string) error {
	_, err := s.db.Exec(`UPDATE builds SET
		package = CASE WHEN package = '' THEN ? ELSE package END,
		version = CASE WHEN version = '' THEN ? ELSE version END
		WHERE id = ?`, pkg, version, id)
	return err
}

// AddBuildInstall records one install attempt.
func (s *Store) AddBuildInstall(in model.BuildInstall) error {
	_, err := s.db.Exec(`INSERT INTO build_installs
		(build_id, device_id, device_model, os_version, user, via, ref, status, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.BuildID, in.DeviceID, in.DeviceModel, in.OSVersion, in.User, in.Via, in.Ref,
		in.Status, in.Detail, in.CreatedAt)
	return err
}

const buildCols = `b.id, b.sha256, b.filename, b.platform, b.format, b.size, b.package, b.app_name,
	b.version, b.build_code, b.min_os, b.path, b.uploaded_by, b.uploaded_at, b.via, b.source_url,
	b.note, b.deleted_at,
	(SELECT COUNT(*) FROM build_installs i WHERE i.build_id = b.id),
	(SELECT COUNT(*) FROM build_installs i WHERE i.build_id = b.id AND i.status = 'ok'),
	(SELECT MAX(created_at) FROM build_installs i WHERE i.build_id = b.id),
	(SELECT COUNT(DISTINCT device_id) FROM build_installs i WHERE i.build_id = b.id)`

func scanBuild(sc interface{ Scan(...any) error }) (model.Build, error) {
	var (
		b       model.Build
		deleted sql.NullTime
		last    any
	)
	err := sc.Scan(&b.ID, &b.SHA256, &b.Filename, &b.Platform, &b.Format, &b.Size, &b.Package,
		&b.AppName, &b.Version, &b.BuildCode, &b.MinOS, &b.Path, &b.UploadedBy, &b.UploadedAt,
		&b.Via, &b.SourceURL, &b.Note, &deleted,
		&b.Installs, &b.InstallsOK, &last, &b.Devices)
	if err != nil {
		return model.Build{}, err
	}
	if deleted.Valid {
		b.DeletedAt = &deleted.Time
	}
	// MAX() loses the column's type, so the driver may hand back text
	switch v := last.(type) {
	case time.Time:
		b.LastInstalled = &v
	case string:
		if t, ok := parseSQLiteTime(v); ok {
			b.LastInstalled = &t
		}
	case []byte:
		if t, ok := parseSQLiteTime(string(v)); ok {
			b.LastInstalled = &t
		}
	}
	return b, nil
}

// parseSQLiteTime reads a timestamp that came back as text. The driver writes
// time.Time in Go's String() form, "2006-01-02 15:04:05.999 +0500 +05
// m=+0.03", whose zone name and monotonic reading time.Parse cannot match —
// keep date, time and offset only.
func parseSQLiteTime(s string) (time.Time, bool) {
	if f := strings.Fields(s); len(f) >= 4 {
		s = strings.Join(f[:3], " ")
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Build loads one build, deleted or not.
func (s *Store) Build(id int64) (model.Build, error) {
	return scanBuild(s.db.QueryRow(`SELECT `+buildCols+` FROM builds b WHERE b.id = ?`, id))
}

func (s *Store) buildBySHA(sha string) (model.Build, error) {
	return scanBuild(s.db.QueryRow(`SELECT `+buildCols+` FROM builds b WHERE b.sha256 = ?`, sha))
}

// BuildFilter narrows ListBuilds. Q matches file name, package, app name,
// version and uploader.
type BuildFilter struct {
	Q        string
	Platform string
	User     string // uploaded_by
	Before   int64  // id cursor for paging: only builds with a smaller id
	Limit    int
}

// ListBuilds returns live builds, newest first.
func (s *Store) ListBuilds(f BuildFilter) ([]model.Build, error) {
	where := []string{"b.deleted_at IS NULL"}
	var args []any
	if f.Q != "" {
		q := "%" + strings.ToLower(f.Q) + "%"
		where = append(where, `(lower(b.filename) LIKE ? OR lower(b.package) LIKE ? OR lower(b.app_name) LIKE ?
			OR lower(b.version) LIKE ? OR lower(b.uploaded_by) LIKE ? OR lower(b.note) LIKE ?)`)
		args = append(args, q, q, q, q, q, q)
	}
	if f.Platform != "" {
		where = append(where, "b.platform = ?")
		args = append(args, f.Platform)
	}
	if f.User != "" {
		where = append(where, "b.uploaded_by = ?")
		args = append(args, f.User)
	}
	if f.Before > 0 {
		where = append(where, "b.id < ?")
		args = append(args, f.Before)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	args = append(args, f.Limit)
	rows, err := s.db.Query(`SELECT `+buildCols+` FROM builds b WHERE `+strings.Join(where, " AND ")+
		` ORDER BY b.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Build{}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BuildInstalls lists a build's installs, newest first.
func (s *Store) BuildInstalls(buildID int64, limit int) ([]model.BuildInstall, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, build_id, device_id, device_model, os_version, user, via, ref,
		status, detail, created_at FROM build_installs WHERE build_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, buildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.BuildInstall{}
	for rows.Next() {
		var in model.BuildInstall
		if err := rows.Scan(&in.ID, &in.BuildID, &in.DeviceID, &in.DeviceModel, &in.OSVersion,
			&in.User, &in.Via, &in.Ref, &in.Status, &in.Detail, &in.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SetBuildNote replaces a build's free-text note.
func (s *Store) SetBuildNote(id int64, note string) error {
	_, err := s.db.Exec(`UPDATE builds SET note = ? WHERE id = ?`, note, id)
	return err
}

// DeleteBuild marks a build deleted; its install history stays.
func (s *Store) DeleteBuild(id int64) error {
	_, err := s.db.Exec(`UPDATE builds SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, time.Now(), id)
	return err
}

// BuildsSize is the total size of live builds on disk.
func (s *Store) BuildsSize() (count int, bytes int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM builds WHERE deleted_at IS NULL`).
		Scan(&count, &bytes)
	return count, bytes, err
}
