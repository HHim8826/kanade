package library

import (
	"context"
	"database/sql"
	"regexp"

	"golang.org/x/text/unicode/norm"
)

// Variants: a song that is another's instrumental, or a shortened one (TV size), as its title or
// version note says. Many albums of anime songs end with them; shuffling can leave them out.
const (
	VariantInstrumental = "instrumental"
	VariantShort        = "short"
)

var (
	instrumental = regexp.MustCompile(`(?i)\binst(?:rumental)?\b|\boff[\s_.-]*vo(?:cal|\.|\b)|\bwithout[\s_-]*vocals?\b|` +
		`\bkaraoke\b|\bbacking[\s_-]*tracks?\b|\bminus[\s_-]*one\b|カラオケ|オフ[・\s]*ボ[ーｰ]カル|インスト|伴奏|純音樂|纯音乐`)
	short = regexp.MustCompile(`(?i)\btv[\s_.-]*(?:size|ver(?:sion)?\b|edit\b|mix\b|length\b)|tv[\s・]*サイズ|` +
		`\bshort[\s_.-]*(?:ver(?:sion)?\b|size\b|edit\b)|ショート[\s・]*(?:ver|バージョン|サイズ)|\b(?:movie|game|anime)[\s_.-]*size\b`)
)

// Variant is what a song is by its title and version note: VariantInstrumental, VariantShort or "".
func Variant(title, version string) string {
	s := norm.NFKC.String(title + "\n" + version)
	switch {
	case instrumental.MatchString(s):
		return VariantInstrumental
	case short.MatchString(s):
		return VariantShort
	}
	return ""
}

// setVariant brings a track's variant up to date with its title and version note.
func setVariant(ctx context.Context, tx *sql.Tx, id int64) error {
	var title, version string
	if err := tx.QueryRowContext(ctx, `SELECT title, version FROM tracks WHERE id = ?`, id).Scan(&title, &version); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE tracks SET variant = ?1 WHERE id = ?2 AND variant != ?1`, Variant(title, version), id)
	return err
}

// variantsVersion changes whenever Variant does; EnsureVariants then tells every song again.
const variantsVersion = "1"

// EnsureVariants tells every song's variant when Variant has changed since it last did (and the
// first time, for the songs from before variants).
func (s *Store) EnsureVariants(ctx context.Context) error {
	var v string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'variants'`).Scan(&v)
	if v == variantsVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, title, version, variant FROM tracks`)
	if err != nil {
		return err
	}
	changed := map[int64]string{}
	for rows.Next() {
		var id int64
		var title, version, was string
		if err := rows.Scan(&id, &title, &version, &was); err != nil {
			rows.Close()
			return err
		}
		if now := Variant(title, version); now != was {
			changed[id] = now
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, v := range changed {
		if _, err := tx.ExecContext(ctx, `UPDATE tracks SET variant = ? WHERE id = ?`, v, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('variants', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, variantsVersion); err != nil {
		return err
	}
	return tx.Commit()
}
