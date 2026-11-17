package main

import "database/sql"

// seedRequest supplies the range explicitly; no migration trigger is assumed.
func seedRequest(d *sql.DB, query string, args ...any) (sql.Result, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, args...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO request_ranges(request_id,start_date,end_date) SELECT id,start_date,end_date FROM requests WHERE id=?`, id); err != nil {
		return nil, err
	}
	return res, tx.Commit()
}
