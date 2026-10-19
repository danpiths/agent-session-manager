package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type rowFilter struct {
	Where string
	Args  []any
}

func readOnlyDB(filename string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filename, RawQuery: "mode=ro"}
	return sql.Open("sqlite", u.String())
}

func databaseTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Preserve the native schema and migrations, but retain only selected records.
// VACUUM removes discarded rows from free pages before the snapshot is exported.
func snapshotDatabase(filename string, keep map[string]rowFilter) ([]byte, error) {
	source, err := readOnlyDB(filename)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	dir, err := os.MkdirTemp("", "asm-db-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	copyPath := filepath.Join(dir, "snapshot.sqlite")
	if _, err = source.Exec("VACUUM INTO ?", copyPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", copyPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err = db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return nil, err
	}
	names, err := databaseTables(db)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		query := "DELETE FROM " + quoteIdentifier(name)
		filter, ok := keep[name]
		if ok {
			query += " WHERE NOT COALESCE((" + filter.Where + "),0)"
		}
		if _, err = db.Exec(query, filter.Args...); err != nil {
			return nil, err
		}
	}
	if _, err = db.Exec("VACUUM"); err != nil {
		return nil, err
	}
	if err = db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(copyPath)
}

func temporaryDatabase(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "asm-restore-*.sqlite")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err = f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err = f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// Merge native rows transactionally. Conflicting primary keys cause a failure,
// rather than silently replacing state belonging to an existing conversation.
func restoreDatabase(filename string, data []byte, relocations map[string]map[string]string, ignoredTables []string) error {
	if len(data) == 0 {
		return nil
	}
	snapshot, cleanup, err := temporaryDatabase(data)
	if err != nil {
		return err
	}
	defer cleanup()
	source, err := sql.Open("sqlite", snapshot)
	if err != nil {
		return err
	}
	defer source.Close()
	for table, columns := range relocations {
		for column, replacement := range columns {
			parts := strings.SplitN(replacement, "\x00", 2)
			if len(parts) != 2 {
				continue
			}
			query := "UPDATE " + quoteIdentifier(table) + " SET " + quoteIdentifier(column) + " = ? || substr(" + quoteIdentifier(column) + ", ?) WHERE substr(" + quoteIdentifier(column) + ",1,?) = ?"
			length := utf8.RuneCountInString(parts[0])
			if _, err = source.Exec(query, parts[1], length+1, length, parts[0]); err != nil {
				return err
			}
		}
	}
	if err = source.Close(); err != nil {
		return err
	}
	if _, err = os.Stat(filename); os.IsNotExist(err) {
		contents, err := os.ReadFile(snapshot)
		if err != nil {
			return err
		}
		_, err = restoreNativeFiles(filepath.Dir(filename), map[string][]byte{"files/" + filepath.Base(filename): contents}, nil, func(rel string) bool { return rel == filepath.Base(filename) }, nil)
		return err
	}
	dest, err := sql.Open("sqlite", filename)
	if err != nil {
		return err
	}
	defer dest.Close()
	dest.SetMaxOpenConns(1)
	u := url.URL{Scheme: "file", Path: snapshot, RawQuery: "mode=ro"}
	if _, err = dest.Exec("ATTACH DATABASE ? AS incoming", u.String()); err != nil {
		return err
	}
	tx, err := dest.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT name FROM incoming.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, table := range tables {
		ignored := false
		for _, name := range ignoredTables {
			if table == name {
				ignored = true
				break
			}
		}
		if ignored {
			continue
		}
		info, err := tx.Query("PRAGMA incoming.table_info(" + quoteIdentifier(table) + ")")
		if err != nil {
			return err
		}
		var columns, keys []string
		for info.Next() {
			var cid, notnull, pk int
			var name, kind string
			var def any
			if err = info.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
				info.Close()
				return err
			}
			columns = append(columns, name)
			if pk > 0 {
				keys = append(keys, name)
			}
		}
		if err = info.Close(); err != nil {
			return err
		}
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM incoming." + quoteIdentifier(table)).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		if len(keys) == 0 {
			return fmt.Errorf("cannot safely merge native table %s without a primary key", table)
		}
		var equal, join, quoted []string
		for _, column := range columns {
			q := quoteIdentifier(column)
			quoted = append(quoted, q)
			equal = append(equal, "a."+q+" IS b."+q)
		}
		for _, key := range keys {
			q := quoteIdentifier(key)
			join = append(join, "a."+q+" IS b."+q)
		}
		query := "SELECT count(*) FROM main." + quoteIdentifier(table) + " a JOIN incoming." + quoteIdentifier(table) + " b ON " + strings.Join(join, " AND ") + " WHERE NOT (" + strings.Join(equal, " AND ") + ")"
		if err = tx.QueryRow(query).Scan(&count); err != nil {
			return fmt.Errorf("native schema mismatch for %s: %w", table, err)
		}
		if count > 0 {
			return fmt.Errorf("native state conflict in %s; existing records were not overwritten", table)
		}
		list := strings.Join(quoted, ",")
		if _, err = tx.Exec("INSERT OR IGNORE INTO main." + quoteIdentifier(table) + " (" + list + ") SELECT " + list + " FROM incoming." + quoteIdentifier(table)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
