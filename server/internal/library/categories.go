package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Categories are folders of the user's own for albums (review #92): browsing a library of several
// works by work. An album can be in several categories; putting it in one or taking it out changes
// nothing of the album itself. What is in a category changes through the edit log, so a batch of it
// is one edit that undo takes back.

type Category struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	ParentID int64   `json:"parent_id,omitempty"` // a category in another (not shown yet)
	Albums   int     `json:"albums"`              // albums in it that are in the lists
	Covers   []int64 `json:"covers"`              // a few of their covers, the latest added first
	Selected int     `json:"selected"`            // of the albums asked about, how many are in it
}

type CategoryBrief struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type CategoryList struct {
	Categories    []Category `json:"categories"`
	Uncategorized int        `json:"uncategorized"` // albums in the lists that are in no category
	Covers        []int64    `json:"uncategorized_covers"`
}

// categoryRow is a category as a "row" edit stores it, with the albums it held, so a deleted one can
// be put back as it was.
type categoryRow struct {
	Name      string  `json:"name"`
	ParentID  int64   `json:"parent_id,omitempty"`
	CreatedAt int64   `json:"created_at"`
	Albums    []int64 `json:"albums"`
}

const maxBatch = 10000 // albums in one categorizing

const albumListed = `EXISTS (SELECT 1 FROM album_entries le WHERE le.album_id = al.id)`

// cleanCategoryName is a category's name as stored: trimmed, at most 100 characters.
func cleanCategoryName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", invalid("a category needs a name")
	}
	if utf8.RuneCountInString(name) > 100 {
		return "", invalid("a category name is at most 100 characters")
	}
	return name, nil
}

func categoryNameTaken(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return invalid("a category of that name exists")
	}
	return err
}

// Categories lists the categories by name, with how many of the given albums each holds.
func (s *Store) Categories(ctx context.Context, albums []int64) (*CategoryList, error) {
	out := &CategoryList{Categories: []Category{}, Covers: []int64{}}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.name, coalesce(c.parent_id, 0),
		(SELECT count(*) FROM album_categories ac JOIN albums al ON al.id = ac.album_id WHERE ac.category_id = c.id AND `+albumListed+`)
		FROM categories c ORDER BY c.name COLLATE NOCASE, c.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		c := Category{Covers: []int64{}}
		if err := rows.Scan(&c.ID, &c.Name, &c.ParentID, &c.Albums); err != nil {
			rows.Close()
			return nil, err
		}
		out.Categories = append(out.Categories, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Categories {
		c := &out.Categories[i]
		if c.Covers, err = idsTx(ctx, s.db, `SELECT al.cover_id FROM album_categories ac JOIN albums al ON al.id = ac.album_id
			WHERE ac.category_id = ? AND al.cover_id IS NOT NULL AND `+albumListed+` ORDER BY ac.added_at DESC, al.id DESC LIMIT 4`, c.ID); err != nil {
			return nil, err
		}
		if c.Covers == nil {
			c.Covers = []int64{}
		}
		if len(albums) > 0 {
			args := []any{c.ID}
			for _, a := range albums {
				args = append(args, a)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM album_categories WHERE category_id = ? AND album_id IN (?`+
				strings.Repeat(", ?", len(albums)-1)+`)`, args...).Scan(&c.Selected); err != nil {
				return nil, err
			}
		}
	}
	none := `FROM albums al WHERE ` + albumListed + ` AND NOT EXISTS (SELECT 1 FROM album_categories ac WHERE ac.album_id = al.id)`
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) `+none).Scan(&out.Uncategorized); err != nil {
		return nil, err
	}
	if out.Covers, err = idsTx(ctx, s.db, `SELECT al.cover_id `+none+` AND al.cover_id IS NOT NULL ORDER BY al.id DESC LIMIT 4`); err != nil {
		return nil, err
	}
	if out.Covers == nil {
		out.Covers = []int64{}
	}
	return out, nil
}

// CreateCategory makes an empty category.
func (s *Store) CreateCategory(ctx context.Context, name string) (*Category, error) {
	name, err := cleanCategoryName(name)
	if err != nil {
		return nil, err
	}
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO categories (name, created_at, updated_at) VALUES (?, ?, ?)`, name, now, now)
	if err != nil {
		return nil, categoryNameTaken(err)
	}
	id, _ := r.LastInsertId()
	return &Category{ID: id, Name: name, Covers: []int64{}}, nil
}

// RenameCategory gives a category another name.
func (s *Store) RenameCategory(ctx context.Context, id int64, name string) error {
	name, err := cleanCategoryName(name)
	if err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE categories SET name = ?, updated_at = ? WHERE id = ?`, name, db.Now(), id)
	if err != nil {
		return categoryNameTaken(err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: category %d", ErrNotFound, id)
	}
	return nil
}

// DeleteCategory removes a category, as one edit that undo takes back with the albums it held; the
// albums themselves stay as they are.
func (s *Store) DeleteCategory(ctx context.Context, id int64) (int64, error) {
	name, err := s.name(ctx, "category", id)
	if err != nil {
		return 0, err
	}
	return s.edit(ctx, SourceUser, "刪除分類「"+name+"」", func(e *editor) error {
		cur, _, err := e.current("category", id, "row")
		if err != nil {
			return err
		}
		if err := e.write("category", id, "row", nil); err != nil {
			return err
		}
		return e.record("category", id, "row", cur, nil)
	})
}

// CategorizeRequest puts albums into categories and takes them out of others, as one edit; Create
// makes a new category in the same edit and puts the albums into it.
type CategorizeRequest struct {
	Albums []int64 `json:"albums"`
	Add    []int64 `json:"add"`
	Remove []int64 `json:"remove"`
	Create string  `json:"create"`
}

// Categorize applies a CategorizeRequest after checking every album and category.
func (s *Store) Categorize(ctx context.Context, req CategorizeRequest) (group int64, created *Category, err error) {
	albums := uniqueIDs(req.Albums)
	if len(albums) == 0 || len(albums) > maxBatch {
		return 0, nil, invalid("give from 1 to %d albums", maxBatch)
	}
	add, remove := uniqueIDs(req.Add), uniqueIDs(req.Remove)
	for _, id := range add {
		if slices.Contains(remove, id) {
			return 0, nil, invalid("category %d is both added and removed", id)
		}
	}
	create := strings.TrimSpace(req.Create)
	if create != "" {
		if create, err = cleanCategoryName(create); err != nil {
			return 0, nil, err
		}
	}
	if len(add) == 0 && len(remove) == 0 && create == "" {
		return 0, nil, invalid("nothing to change")
	}
	names := map[int64]string{}
	for _, id := range append(slices.Clone(add), remove...) {
		n, err := s.name(ctx, "category", id)
		if err != nil {
			return 0, nil, err
		}
		names[id] = n
	}
	for _, id := range albums {
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return 0, nil, fmt.Errorf("%w: album %d", ErrNotFound, id)
		} else if err != nil {
			return 0, nil, err
		}
	}
	summary := fmt.Sprintf("調整 %d 張專輯的分類", len(albums))
	switch {
	case create != "" && len(add) == 0 && len(remove) == 0:
		summary = fmt.Sprintf("將 %d 張專輯加入新分類「%s」", len(albums), create)
	case create == "" && len(add) == 1 && len(remove) == 0:
		summary = fmt.Sprintf("將 %d 張專輯加入「%s」", len(albums), names[add[0]])
	case create == "" && len(add) == 0 && len(remove) == 1:
		summary = fmt.Sprintf("將 %d 張專輯移出「%s」", len(albums), names[remove[0]])
	}
	group, err = s.edit(ctx, SourceUser, summary, func(e *editor) error {
		if create != "" {
			now := db.Now()
			r, err := e.tx.ExecContext(e.ctx, `INSERT INTO categories (name, created_at, updated_at) VALUES (?, ?, ?)`, create, now, now)
			if err != nil {
				return categoryNameTaken(err)
			}
			id, _ := r.LastInsertId()
			row, _, err := e.current("category", id, "row")
			if err != nil {
				return err
			}
			if err := e.record("category", id, "row", nil, row); err != nil {
				return err
			}
			created = &Category{ID: id, Name: create, Covers: []int64{}}
			add = append(add, id)
		}
		for _, album := range albums {
			cur, err := e.albumCategories(album)
			if err != nil {
				return err
			}
			next := slices.DeleteFunc(slices.Clone(cur), func(id int64) bool { return slices.Contains(remove, id) })
			for _, id := range add {
				if !slices.Contains(next, id) {
					next = append(next, id)
				}
			}
			if err := e.setCategories(album, next); err != nil {
				return err
			}
		}
		return nil
	})
	if group == 0 {
		created = nil
	}
	return group, created, err
}

func uniqueIDs(in []int64) []int64 {
	out := []int64{}
	for _, id := range in {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// albumCategories are the categories an album is in, by number.
func (e *editor) albumCategories(album int64) ([]int64, error) {
	return idsTx(e.ctx, e.tx, `SELECT category_id FROM album_categories WHERE album_id = ? ORDER BY category_id`, album)
}

func encodeIDs(ids []int64) string {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, "\n")
}

func decodeIDs(v string) []int64 {
	var out []int64
	for _, p := range strings.Split(v, "\n") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// setCategories puts an album in exactly these categories, recorded when that changes anything.
func (e *editor) setCategories(album int64, ids []int64) error {
	cur, _, err := e.current("album", album, "categories")
	if err != nil {
		return err
	}
	v := Str(encodeIDs(ids))
	if same(cur, v) {
		return nil
	}
	if err := e.write("album", album, "categories", v); err != nil {
		return err
	}
	return e.record("album", album, "categories", cur, v)
}

// moveCategories has an album emptied into another pass its categories on (review #92): the target
// is in all of them besides its own, the emptied album in none; undo puts them back.
func (e *editor) moveCategories(from, to int64) error {
	moving, err := e.albumCategories(from)
	if err != nil || len(moving) == 0 {
		return err
	}
	have, err := e.albumCategories(to)
	if err != nil {
		return err
	}
	for _, id := range moving {
		if !slices.Contains(have, id) {
			have = append(have, id)
		}
	}
	if err := e.setCategories(to, have); err != nil {
		return err
	}
	return e.setCategories(from, nil)
}

// writeCategories stores an album's categories (only those that still exist).
func (e *editor) writeCategories(album int64, v *string) error {
	if _, err := e.tx.ExecContext(e.ctx, `DELETE FROM album_categories WHERE album_id = ?`, album); err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	now := db.Now()
	for _, id := range decodeIDs(*v) {
		if _, err := e.tx.ExecContext(e.ctx, `INSERT OR IGNORE INTO album_categories (category_id, album_id, added_at)
			SELECT id, ?, ? FROM categories WHERE id = ?`, album, now, id); err != nil {
			return err
		}
	}
	return nil
}

// categoryRowNow reads a category as a "row" edit stores it (nil when there is none).
func (e *editor) categoryRowNow(id int64) (*string, error) {
	var r categoryRow
	err := e.tx.QueryRowContext(e.ctx, `SELECT name, coalesce(parent_id, 0), created_at FROM categories WHERE id = ?`, id).
		Scan(&r.Name, &r.ParentID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Albums, err = idsTx(e.ctx, e.tx, `SELECT album_id FROM album_categories WHERE category_id = ? ORDER BY album_id`, id); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(r)
	return Str(string(b)), nil
}

// writeCategoryRow removes a category (nil) or puts one back as it was, with the albums it held that
// still exist.
func (e *editor) writeCategoryRow(id int64, v *string) error {
	if v == nil {
		_, err := e.tx.ExecContext(e.ctx, `DELETE FROM categories WHERE id = ?`, id)
		return err
	}
	var r categoryRow
	if err := json.Unmarshal([]byte(*v), &r); err != nil {
		return err
	}
	var parent any
	if r.ParentID != 0 {
		parent = r.ParentID
	}
	now := db.Now()
	if _, err := e.tx.ExecContext(e.ctx, `INSERT INTO categories (id, name, parent_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, r.Name, parent, r.CreatedAt, now); err != nil {
		return categoryNameTaken(err)
	}
	for _, album := range r.Albums {
		if _, err := e.tx.ExecContext(e.ctx, `INSERT OR IGNORE INTO album_categories (category_id, album_id, added_at)
			SELECT ?, id, ? FROM albums WHERE id = ?`, id, now, album); err != nil {
			return err
		}
	}
	return nil
}

// categoryNames names categories for the edit log, in order of name.
func (s *Store) categoryNames(ctx context.Context, v *string) string {
	if v == nil || *v == "" {
		return "（無）"
	}
	var names []string
	for _, id := range decodeIDs(*v) {
		n, err := s.name(ctx, "category", id)
		if err != nil {
			n = "（已刪除的分類）"
		}
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, "、")
}

// AlbumCategories are the categories an album is in, by name.
func (s *Store) AlbumCategories(ctx context.Context, album int64) ([]CategoryBrief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.name FROM album_categories ac JOIN categories c ON c.id = ac.category_id
		WHERE ac.album_id = ? ORDER BY c.name COLLATE NOCASE`, album)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CategoryBrief{}
	for rows.Next() {
		var c CategoryBrief
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
