package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type kind int

const (
	kindString kind = iota
	kindInt
	kindFloat
)

type column struct {
	name     string
	kind     kind
	nullable bool
	primary  bool
	autoInc  bool
	hasDef   bool
}

type table struct {
	name    string
	columns []*column
	byName  map[string]*column
	pk      *column
}

// explorer живёт в замыкании NewDBExplorer: глобальных переменных нет.
type explorer struct {
	db     *sql.DB
	names  []string
	tables map[string]*table
}

type result map[string]interface{}

func NewDBExplorer(db *sql.DB) (http.Handler, error) {
	e := &explorer{db: db, tables: map[string]*table{}}

	rows, err := db.Query("SHOW TABLES")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		e.names = append(e.names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, name := range e.names {
		t, err := e.loadTable(name)
		if err != nil {
			return nil, err
		}
		e.tables[name] = t
	}

	return http.HandlerFunc(e.serve), nil
}

func (e *explorer) loadTable(name string) (*table, error) {
	rows, err := e.db.Query("SHOW FULL COLUMNS FROM `" + strings.ReplaceAll(name, "`", "``") + "`")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t := &table{name: name, byName: map[string]*column{}}
	for rows.Next() {
		var field, typ, null, key, extra string
		var collation, def, privileges, comment sql.NullString
		if err := rows.Scan(&field, &typ, &collation, &null, &key, &def, &extra, &privileges, &comment); err != nil {
			return nil, err
		}
		c := &column{
			name:     field,
			nullable: null == "YES",
			primary:  key == "PRI",
			autoInc:  strings.Contains(extra, "auto_increment"),
			hasDef:   def.Valid,
			kind:     kindOf(typ),
		}
		t.columns = append(t.columns, c)
		t.byName[field] = c
		if c.primary && t.pk == nil {
			t.pk = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if t.pk == nil && len(t.columns) > 0 {
		t.pk = t.columns[0]
	}
	return t, nil
}

func kindOf(typ string) kind {
	typ = strings.ToLower(typ)
	switch {
	case strings.HasPrefix(typ, "tinyint"), strings.HasPrefix(typ, "smallint"),
		strings.HasPrefix(typ, "mediumint"), strings.HasPrefix(typ, "int"),
		strings.HasPrefix(typ, "bigint"):
		return kindInt
	case strings.HasPrefix(typ, "float"), strings.HasPrefix(typ, "double"),
		strings.HasPrefix(typ, "decimal"), strings.HasPrefix(typ, "numeric"):
		return kindFloat
	}
	return kindString
}

// valid проверяет значение из JSON на соответствие типу колонки.
func (c *column) valid(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return c.nullable
	case string:
		return c.kind == kindString
	case float64:
		if c.kind == kindFloat {
			return true
		}
		return c.kind == kindInt && x == float64(int64(x))
	}
	return false
}

func (c *column) zero() interface{} {
	if c.kind == kindString {
		return ""
	}
	return 0
}

// convert приводит значение, прочитанное из базы, к типу JSON.
func (c *column) convert(v interface{}) interface{} {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	s := string(b)
	switch c.kind {
	case kindInt:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case kindFloat:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}

func (e *explorer) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}

	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, result{"response": result{"tables": e.names}})
		return
	}
	if len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	t, ok := e.tables[parts[0]]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown table")
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			e.list(w, r, t)
		case http.MethodPut:
			e.create(w, r, t)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		e.get(w, t, parts[1])
	case http.MethodPost:
		e.update(w, r, t, parts[1])
	case http.MethodDelete:
		e.remove(w, t, parts[1])
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (e *explorer) list(w http.ResponseWriter, r *http.Request, t *table) {
	limit := intParam(r, "limit", 5)
	offset := intParam(r, "offset", 0)

	records, err := e.query(t, "SELECT "+columnList(t)+" FROM "+quote(t.name)+" LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result{"response": result{"records": records}})
}

func (e *explorer) get(w http.ResponseWriter, t *table, id string) {
	records, err := e.query(t, "SELECT "+columnList(t)+" FROM "+quote(t.name)+" WHERE "+quote(t.pk.name)+" = ?", id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(records) == 0 {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	writeJSON(w, http.StatusOK, result{"response": result{"record": records[0]}})
}

func (e *explorer) create(w http.ResponseWriter, r *http.Request, t *table) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}

	var names []string
	var args []interface{}
	for _, c := range t.columns {
		if c.autoInc {
			continue
		}
		v, present := body[c.name]
		if present {
			if !c.valid(v) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("field %s have invalid type", c.name))
				return
			}
		} else {
			if c.nullable || c.hasDef {
				continue
			}
			v = c.zero()
		}
		names = append(names, quote(c.name))
		args = append(args, v)
	}

	var q string
	if len(names) == 0 {
		q = "INSERT INTO " + quote(t.name) + " () VALUES ()"
	} else {
		q = "INSERT INTO " + quote(t.name) + " (" + strings.Join(names, ", ") + ") VALUES (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
	}
	res, err := e.db.Exec(q, args...)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result{"response": result{t.pk.name: id}})
}

func (e *explorer) update(w http.ResponseWriter, r *http.Request, t *table, id string) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}

	var sets []string
	var args []interface{}
	for _, c := range t.columns {
		v, present := body[c.name]
		if !present {
			continue
		}
		if c.primary || !c.valid(v) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("field %s have invalid type", c.name))
			return
		}
		sets = append(sets, quote(c.name)+" = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		writeJSON(w, http.StatusOK, result{"response": result{"updated": 0}})
		return
	}

	args = append(args, id)
	res, err := e.db.Exec("UPDATE "+quote(t.name)+" SET "+strings.Join(sets, ", ")+" WHERE "+quote(t.pk.name)+" = ?", args...)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result{"response": result{"updated": n}})
}

func (e *explorer) remove(w http.ResponseWriter, t *table, id string) {
	res, err := e.db.Exec("DELETE FROM "+quote(t.name)+" WHERE "+quote(t.pk.name)+" = ?", id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result{"response": result{"deleted": n}})
}

// query выполняет запрос и динамически собирает записи по колонкам таблицы.
func (e *explorer) query(t *table, q string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := e.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []map[string]interface{}{}
	for rows.Next() {
		vals := make([]interface{}, len(t.columns))
		ptrs := make([]interface{}, len(t.columns))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make(map[string]interface{}, len(t.columns))
		for i, c := range t.columns {
			rec[c.name] = c.convert(vals[i])
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func columnList(t *table) string {
	names := make([]string, len(t.columns))
	for i, c := range t.columns {
		names[i] = quote(c.name)
	}
	return strings.Join(names, ", ")
}

func quote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func intParam(r *http.Request, key string, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || n < 0 {
		return def
	}
	return n
}

func readBody(w http.ResponseWriter, r *http.Request) (map[string]interface{}, bool) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return nil, false
	}
	return body, true
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, result{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}
