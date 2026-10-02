package gdrive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
)

// RootFolderName is the platform folder in the root of the connected Drive.
// Every write the service makes stays inside it (decision D1).
const RootFolderName = "Kanade"

const settingRootID = "drive_root_id"

type File struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	MimeType       string   `json:"mimeType"`
	Size           string   `json:"size"`
	Parents        []string `json:"parents"`
	Trashed        bool     `json:"trashed"`
	MD5Checksum    string   `json:"md5Checksum"`
	SHA256Checksum string   `json:"sha256Checksum"`
}

func (f File) SizeBytes() int64 { n, _ := strconv.ParseInt(f.Size, 10, 64); return n }

func MediaURL(id string) string {
	return apiBase + "/files/" + url.PathEscape(id) + "?alt=media&acknowledgeAbuse=true"
}

func (c *Client) GetFile(ctx context.Context, id, fields string) (File, error) {
	var f File
	err := c.Call(ctx, http.MethodGet, apiBase+"/files/"+url.PathEscape(id)+"?"+url.Values{"fields": {fields}}.Encode(), nil, &f)
	return f, err
}

type About struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	LimitBytes int64  `json:"limit_bytes"` // 0 when unlimited
	UsageBytes int64  `json:"usage_bytes"`
}

func (c *Client) About(ctx context.Context) (About, error) {
	var a struct {
		User struct {
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
		} `json:"storageQuota"`
	}
	if err := c.Call(ctx, http.MethodGet, apiBase+"/about?fields=user(displayName,emailAddress),storageQuota(limit,usage)", nil, &a); err != nil {
		return About{}, err
	}
	limit, _ := strconv.ParseInt(a.StorageQuota.Limit, 10, 64)
	usage, _ := strconv.ParseInt(a.StorageQuota.Usage, 10, 64)
	return About{Email: a.User.EmailAddress, Name: a.User.DisplayName, LimitBytes: limit, UsageBytes: usage}, nil
}

// quote escapes a value for a Drive search query.
func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func (c *Client) findFolder(ctx context.Context, parent, name string) (string, error) {
	q := fmt.Sprintf("name = %s and mimeType = '%s' and %s in parents and trashed = false", quote(name), FolderMime, quote(parent))
	var list struct{ Files []File }
	if err := c.Call(ctx, http.MethodGet, apiBase+"/files?"+url.Values{"q": {q}, "fields": {"files(id)"}, "pageSize": {"10"}}.Encode(), nil, &list); err != nil {
		return "", err
	}
	if len(list.Files) == 0 {
		return "", nil
	}
	return list.Files[0].ID, nil
}

func (c *Client) createFolder(ctx context.Context, parent, name string) (string, error) {
	var f File
	err := c.Call(ctx, http.MethodPost, apiBase+"/files?fields=id",
		map[string]any{"name": name, "mimeType": FolderMime, "parents": []string{parent}}, &f)
	return f.ID, err
}

// Root returns the platform folder, creating it on first use. If it was trashed or deleted
// in Drive, a new one is found or created.
func (c *Client) Root(ctx context.Context) (string, error) {
	id, err := db.GetSetting(ctx, c.db, settingRootID)
	if err != nil {
		return "", err
	}
	if id != "" {
		f, err := c.GetFile(ctx, id, "id,name,trashed")
		if err == nil && !f.Trashed {
			if f.Name != RootFolderName { // the product was renamed; the folder follows by ID
				if err := c.Call(ctx, http.MethodPatch, apiBase+"/files/"+url.PathEscape(id)+"?fields=id",
					map[string]string{"name": RootFolderName}, nil); err != nil {
					return "", err
				}
			}
			return id, nil
		}
		if err != nil && !IsNotFound(err) {
			return "", err
		}
		// Gone: forget it and every subfolder cached under it.
		if _, err := c.db.ExecContext(ctx, `DELETE FROM drive_folders`); err != nil {
			return "", err
		}
	}
	if id, err = c.findFolder(ctx, "root", RootFolderName); err != nil {
		return "", err
	}
	if id == "" {
		if id, err = c.createFolder(ctx, "root", RootFolderName); err != nil {
			return "", err
		}
	}
	return id, db.SetSetting(ctx, c.db, settingRootID, id)
}

// Folder returns the ID of a folder path below the platform root ("library/Artist/Album"),
// creating missing levels. IDs are cached in drive_folders.
func (c *Client) Folder(ctx context.Context, path string) (string, error) {
	parent, err := c.Root(ctx)
	if err != nil {
		return "", err
	}
	rel := ""
	for _, name := range strings.Split(strings.Trim(path, "/"), "/") {
		if name == "" {
			continue
		}
		if rel != "" {
			rel += "/"
		}
		rel += name
		var id string
		err := c.db.QueryRowContext(ctx, `SELECT file_id FROM drive_folders WHERE path = ?`, rel).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if id == "" {
			if id, err = c.findFolder(ctx, parent, name); err != nil {
				return "", err
			}
			if id == "" {
				if id, err = c.createFolder(ctx, parent, name); err != nil {
					return "", err
				}
			}
			if _, err := c.db.ExecContext(ctx, `INSERT OR REPLACE INTO drive_folders (path, file_id) VALUES (?, ?)`, rel, id); err != nil {
				return "", err
			}
		}
		parent = id
	}
	return parent, nil
}
