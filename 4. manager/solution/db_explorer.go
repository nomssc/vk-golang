package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// kind — укрупнённый тип колонки. Для валидации достаточно трёх вариантов:
// «string — int — float» (плюс null, который определяется отдельно).
type kind int

const (
	kindString kind = iota
	kindInt
	kindFloat
)

// column описывает одну колонку таблицы. Заполняется один раз при
// инициализации из результата SHOW FULL COLUMNS.
type column struct {
	name     string
	kind     kind
	nullable bool // допускает NULL
	primary  bool // входит в PRIMARY KEY
	autoInc  bool // AUTO_INCREMENT: при вставке значение генерирует база
	hasDef   bool // у колонки есть значение по умолчанию
}

// table описывает таблицу: колонки в порядке объявления, индекс по имени
// и первичный ключ, по которому работают маршруты /$table/$id.
type table struct {
	name    string
	columns []*column
	byName  map[string]*column
	pk      *column
}

// explorer хранит всё состояние сервиса. Он живёт в замыкании
// (метод serve передаётся в http.HandlerFunc), поэтому глобальные
// переменные не нужны.
type explorer struct {
	db     *sql.DB
	names  []string          // имена таблиц в порядке SHOW TABLES
	tables map[string]*table // схема каждой таблицы по имени
}

// result — удобный псевдоним для JSON-объектов в ответах.
type result map[string]interface{}

// NewDBExplorer читает из базы список таблиц и их структуру, после чего
// возвращает handler, который работает только с этими данными.
// Список таблиц считается неизменным на время работы программы.
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
	// Закрываем rows до следующих запросов, чтобы не держать лишнее соединение.
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

// loadTable загружает структуру одной таблицы через SHOW FULL COLUMNS.
func (e *explorer) loadTable(name string) (*table, error) {
	rows, err := e.db.Query("SHOW FULL COLUMNS FROM " + quote(name))
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
	// Запасной вариант для таблицы без PRIMARY KEY — первая колонка.
	if t.pk == nil && len(t.columns) > 0 {
		t.pk = t.columns[0]
	}
	return t, nil
}

// kindOf определяет kind по SQL-типу колонки (например, "int(11)" или "varchar(255)").
// Всё, что не число, считаем строкой.
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

// valid проверяет значение, пришедшее из JSON, на соответствие типу колонки.
// JSON распаковывает любые числа в float64, поэтому для int-колонки
// дополнительно требуется, чтобы число было целым. nil допустим только
// для колонок, разрешающих NULL.
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

// zero возвращает нулевое значение для колонки: "" для строк и 0 для чисел.
// Используется при вставке, если NOT NULL-поле не передали.
func (c *column) zero() interface{} {
	if c.kind == kindString {
		return ""
	}
	return 0
}

// convert приводит значение, прочитанное из базы, к подходящему типу JSON.
// Драйвер отдаёт текстовые данные как []byte, поэтому числовые колонки
// разбираем в int64/float64, а остальные — в string. NULL (nil) остаётся nil.
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

// serve — ручной роутер. Путь делится на части:
//
//	/                 — список таблиц
//	/$table           — список записей (GET) или создание (PUT)
//	/$table/$id       — чтение (GET), обновление (POST), удаление (DELETE)
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

// list обрабатывает GET /$table?limit=&offset=.
// Некорректные или отрицательные limit и offset заменяются значениями
// по умолчанию (5 и 0); в запрос они попадают только как параметры.
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

// get обрабатывает GET /$table/$id. Если записи нет — 404.
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

// create обрабатывает PUT /$table. Правила вставки:
//   - AUTO_INCREMENT-колонки и неизвестные поля игнорируются;
//   - переданные значения проверяются на тип (в том числе null);
//   - непереданные NOT NULL-поля без значения по умолчанию получают
//     нулевое значение, остальные пропускаются.
//
// В ответе возвращается значение первичного ключа новой записи под
// именем этой колонки.
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

// update обрабатывает POST /$table/$id. Обновляются только переданные
// известные поля. Первичный ключ менять нельзя, как и передавать значение
// неподходящего типа — в обоих случаях 400 "field X have invalid type".
// В ответе — количество изменённых строк.
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

// remove обрабатывает DELETE /$table/$id. В ответе — количество удалённых
// строк (0, если записи не было).
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

// query выполняет SELECT и динамически собирает записи: число и имена
// полей берутся из схемы таблицы, значения читаются в []interface{} и
// приводятся к типам JSON через column.convert.
func (e *explorer) query(t *table, q string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := e.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	// rows обязательно закрываем, иначе будут утекать соединения с базой.
	defer rows.Close()

	// Пустой, но не nil срез, чтобы в JSON получилось [], а не null.
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

// columnList возвращает список колонок таблицы для SELECT в порядке схемы.
func columnList(t *table) string {
	names := make([]string, len(t.columns))
	for i, c := range t.columns {
		names[i] = quote(c.name)
	}
	return strings.Join(names, ", ")
}

// quote экранирует идентификатор (имя таблицы или колонки) обратными
// кавычками. Идентификаторы берутся только из схемы, но экранируем их
// на всякий случай.
func quote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// intParam читает неотрицательное целое из query-параметра.
// Если параметра нет или он некорректен, возвращает def.
func intParam(r *http.Request, key string, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// readBody разбирает JSON-тело запроса. При ошибке сам отвечает 400.
func readBody(w http.ResponseWriter, r *http.Request) (map[string]interface{}, bool) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return nil, false
	}
	return body, true
}

// writeError отправляет ошибку в формате {"error": "..."}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, result{"error": msg})
}

// writeJSON сериализует v и отправляет его с нужным статусом.
// Если сериализация не удалась, отвечает 500.
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
